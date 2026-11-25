// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/alibaba/open-code-review/internal/chatgptauth"
)

const (
	factoryReviewEndpoint         = "https://api.openai.com/v1/responses"
	factoryReviewRequestLimit     = 16 << 20
	factoryReviewResponseLimit    = 32 << 20
	factoryReviewRequestTimeout   = 85 * time.Second
	factoryReviewProcessTimeout   = 89 * time.Second
	factoryReviewHeaderTimeout    = 20 * time.Second
	factoryReviewHandshakeTimeout = 10 * time.Second
)

type reviewExit int

// Private process exit codes shared with Factory. Never include provider or credential text.
const (
	reviewRequest          reviewExit = 20
	reviewSignin           reviewExit = 21
	reviewTransport        reviewExit = 22
	reviewHttp3Xx          reviewExit = 23
	reviewHttp400          reviewExit = 24
	reviewHttp401          reviewExit = 25
	reviewHttp403          reviewExit = 26
	reviewHttp429          reviewExit = 27
	reviewHttp4Xx          reviewExit = 28
	reviewHttp5Xx          reviewExit = 29
	reviewResponse         reviewExit = 30
	reviewOutput           reviewExit = 31
	reviewWatchdog         reviewExit = 32
	reviewTransportTimeout reviewExit = 33
	reviewResponseTimeout  reviewExit = 34
)

func (e reviewExit) Error() string { return "factory review request failed" }

func reviewNetworkFailure(err error, fallback, timedOut reviewExit) reviewExit {
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return timedOut
	}
	return fallback
}

func runFactoryReviewCLI(args []string, in io.Reader, out, errOut io.Writer) int {
	watchdog := time.AfterFunc(factoryReviewProcessTimeout, func() {
		os.Exit(int(reviewWatchdog))
	})
	defer watchdog.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), factoryReviewRequestTimeout)
	defer cancel()

	store, err := chatgptauth.DefaultStore()
	if err != nil {
		err = reviewSignin
	} else {
		err = runFactoryReviewRequest(ctx, args, in, out, store, newFactoryReviewHTTPClient())
	}
	if err != nil {
		_, _ = fmt.Fprintln(errOut, "factory review request failed")
		if stage, ok := err.(reviewExit); ok {
			return int(stage)
		}
		return 1
	}
	return 0
}

func runFactoryReviewRequest(ctx context.Context, args []string, in io.Reader, out io.Writer, store chatgptauth.Store, client *http.Client) error {
	model, err := factoryReviewModel(args)
	if err != nil {
		return reviewRequest
	}
	body, err := readFactoryReviewBody(in)
	if err != nil {
		return reviewRequest
	}
	if err := validateFactoryReviewBody(body, model); err != nil {
		return reviewRequest
	}
	token, err := chatgptauth.FactoryFreshPlanToken(store)
	if err != nil {
		return reviewSignin
	}
	return postFactoryReview(ctx, client, token, body, out)
}

func factoryReviewModel(args []string) (string, error) {
	if len(args) != 3 || args[0] != "factory-review-request" || args[1] != "--model" {
		return "", errors.New("invalid factory review arguments")
	}
	model := args[2]
	if model == "" || len(model) > 256 || !utf8.ValidString(model) || strings.TrimSpace(model) != model {
		return "", errors.New("invalid factory review model")
	}
	for _, r := range model {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", errors.New("invalid factory review model")
		}
	}
	return model, nil
}

func readFactoryReviewBody(in io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(in, factoryReviewRequestLimit+1))
	if err != nil {
		return nil, errors.New("factory review input failed")
	}
	if len(body) == 0 || len(body) > factoryReviewRequestLimit {
		return nil, errors.New("invalid factory review input size")
	}
	return body, nil
}

func validateFactoryReviewBody(body []byte, model string) error {
	request, err := decodeFactoryReviewObject(body)
	if err != nil {
		return err
	}
	var requestModel string
	if raw, ok := request["model"]; !ok || json.Unmarshal(raw, &requestModel) != nil || requestModel != model {
		return errors.New("factory review model mismatch")
	}
	if raw, ok := request["stream"]; !ok || string(bytes.TrimSpace(raw)) != "true" {
		return errors.New("factory review requires streaming")
	}
	if raw, ok := request["store"]; !ok || string(bytes.TrimSpace(raw)) != "false" {
		return errors.New("factory review forbids storage")
	}
	for _, key := range []string{"truncation", "previous_response_id", "conversation", "prompt"} {
		if _, ok := request[key]; ok {
			return errors.New("factory review forbids continuation state")
		}
	}
	if raw, ok := request["input"]; ok {
		hasReference, err := containsFactoryItemReference(raw)
		if err != nil {
			return err
		}
		if hasReference {
			return errors.New("factory review forbids item references")
		}
	}
	return nil
}

func decodeFactoryReviewObject(body []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("invalid factory review JSON")
	}
	request := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid factory review JSON")
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid factory review JSON")
		}
		if _, exists := request[key]; exists {
			return nil, errors.New("duplicate factory review field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("invalid factory review JSON")
		}
		request[key] = value
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil, errors.New("invalid factory review JSON")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("invalid factory review JSON")
	}
	return request, nil
}

func containsFactoryItemReference(raw json.RawMessage) (bool, error) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return false, nil
	}
	for _, item := range items {
		object, err := decodeFactoryReviewObject(item)
		if err != nil {
			return false, errors.New("invalid factory review input item")
		}
		var itemType string
		if value, ok := object["type"]; ok && json.Unmarshal(value, &itemType) == nil && itemType == "item_reference" {
			return true, nil
		}
	}
	return false, nil
}

func postFactoryReview(ctx context.Context, client *http.Client, token string, body []byte, out io.Writer) error {
	if client == nil || token == "" {
		return reviewTransport
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, factoryReviewEndpoint, bytes.NewReader(body))
	if err != nil {
		return reviewRequest
	}
	req.GetBody = nil
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := client.Do(req)
	if err != nil {
		return reviewNetworkFailure(err, reviewTransport, reviewTransportTimeout)
	}
	if resp.Body == nil {
		return reviewResponse
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case 400:
			return reviewHttp400
		case 401:
			return reviewHttp401
		case 403:
			return reviewHttp403
		case 429:
			return reviewHttp429
		}
		switch resp.StatusCode / 100 {
		case 3:
			return reviewHttp3Xx
		case 4:
			return reviewHttp4Xx
		case 5:
			return reviewHttp5Xx
		}
		return reviewResponse
	}
	if err := validateFactoryReviewContentType(resp.Header); err != nil {
		return reviewResponse
	}
	if err := validateFactoryReviewContentEncoding(resp.Header); err != nil {
		return reviewResponse
	}
	if resp.ContentLength > factoryReviewResponseLimit {
		return reviewResponse
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, factoryReviewResponseLimit+1))
	if err != nil || len(payload) == 0 || len(payload) > factoryReviewResponseLimit {
		return reviewNetworkFailure(err, reviewResponse, reviewResponseTimeout)
	}
	n, err := out.Write(payload)
	if err != nil || n != len(payload) {
		return reviewOutput
	}
	return nil
}

func validateFactoryReviewContentType(header http.Header) error {
	values := header.Values("Content-Type")
	if len(values) == 0 {
		return nil
	}
	if len(values) != 1 {
		return errors.New("invalid factory review content type")
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		return errors.New("invalid factory review content type")
	}
	return nil
}

func validateFactoryReviewContentEncoding(header http.Header) error {
	values := header.Values("Content-Encoding")
	if len(values) == 0 {
		return nil
	}
	if len(values) != 1 {
		return errors.New("invalid factory review content encoding")
	}
	value := strings.TrimSpace(values[0])
	if value != "" && !strings.EqualFold(value, "identity") {
		return errors.New("invalid factory review content encoding")
	}
	return nil
}

func newFactoryReviewHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: -1}).DialContext,
		ForceAttemptHTTP2:      false,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		TLSHandshakeTimeout:    factoryReviewHandshakeTimeout,
		ResponseHeaderTimeout:  factoryReviewHeaderTimeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"},
		},
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   factoryReviewRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
