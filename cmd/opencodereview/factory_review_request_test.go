// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/alibaba/open-code-review/internal/chatgptauth"
)

type factoryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f factoryRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFactoryReviewRequestForwardsRawSSEErrorEvent(t *testing.T) {
	store := writeFactoryReviewStore(t, time.Now().Add(time.Hour))
	body := []byte(`{"model":"gpt-factory","stream":true,"store":false,"input":"review"}`)
	upstream := []byte("event: error\ndata: {\"error\":{\"message\":\"synthetic\"}}\n\n")
	client := &http.Client{Transport: factoryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != factoryReviewEndpoint {
			t.Errorf("request = %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Error("missing synthetic authorization")
		}
		if req.Header.Get("Accept-Encoding") != "identity" {
			t.Error("request accepts encoded responses")
		}
		if req.GetBody != nil || !req.Close {
			t.Error("request body is replayable or connection is reusable")
		}
		gotBody, err := io.ReadAll(req.Body)
		if err != nil || !bytes.Equal(gotBody, body) {
			t.Errorf("body = %q, err = %v", gotBody, err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}},
			Body:       io.NopCloser(bytes.NewReader(upstream)),
		}, nil
	})}

	var out bytes.Buffer
	if err := runFactoryReviewRequest(context.Background(), []string{"factory-review-request", "--model", "gpt-factory"}, bytes.NewReader(body), &out, store, client); err != nil {
		t.Fatalf("runFactoryReviewRequest: %v", err)
	}
	if !bytes.Equal(out.Bytes(), upstream) {
		t.Fatalf("output = %q", out.Bytes())
	}
}

func TestFactoryReviewBodyRejectsContinuationAndAmbiguousFields(t *testing.T) {
	tests := map[string]string{
		"store null":          `{"model":"m","stream":true,"store":null}`,
		"duplicate store":     `{"model":"m","stream":true,"store":false,"store":false}`,
		"stored prompt":       `{"model":"m","stream":true,"store":false,"prompt":{"id":"p"}}`,
		"previous response":   `{"model":"m","stream":true,"store":false,"previous_response_id":"r"}`,
		"conversation":        `{"model":"m","stream":true,"store":false,"conversation":"c"}`,
		"truncation":          `{"model":"m","stream":true,"store":false,"truncation":"disabled"}`,
		"item reference":      `{"model":"m","stream":true,"store":false,"input":[{"type":"item_reference","id":"i"}]}`,
		"duplicate item type": `{"model":"m","stream":true,"store":false,"input":[{"type":"input_text","type":"item_reference"}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if err := validateFactoryReviewBody([]byte(body), "m"); err == nil {
				t.Fatal("request was accepted")
			}
		})
	}
}

func TestFactoryReviewRequestRejectsRefreshDueAccountWithoutHTTP(t *testing.T) {
	store := writeFactoryReviewStore(t, time.Now().Add(time.Minute))
	called := false
	client := &http.Client{Transport: factoryRoundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	})}
	body := strings.NewReader(`{"model":"m","stream":true,"store":false}`)
	if err := runFactoryReviewRequest(context.Background(), []string{"factory-review-request", "--model", "m"}, body, io.Discard, store, client); err != reviewSignin {
		t.Fatal("expected sign-in refusal")
	}
	if called {
		t.Fatal("HTTP was attempted")
	}
}

func TestFactoryReviewTimeoutStages(t *testing.T) {
	for _, tt := range []struct {
		response bool
		want     reviewExit
	}{{false, reviewTransportTimeout}, {true, reviewResponseTimeout}} {
		client := &http.Client{Transport: factoryRoundTripFunc(func(*http.Request) (*http.Response, error) {
			if !tt.response {
				return nil, context.DeadlineExceeded
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(iotest.ErrReader(context.DeadlineExceeded))}, nil
		})}
		if err := postFactoryReview(context.Background(), client, "synthetic", nil, io.Discard); err != tt.want {
			t.Fatalf("got %v, want stage %d", err, tt.want)
		}
	}
}

func TestFactoryReviewHTTPStage(t *testing.T) {
	client := &http.Client{Transport: factoryRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("private provider detail"))}, nil
	})}
	var output bytes.Buffer
	err := postFactoryReview(context.Background(), client, "synthetic-access", []byte("{}"), &output)
	if err != reviewHttp401 || output.Len() != 0 {
		t.Fatal("expected status-only refusal and no output")
	}
}

func writeFactoryReviewStore(t *testing.T, expiresAt time.Time) chatgptauth.Store {
	t.Helper()
	store := chatgptauth.Store{Dir: t.TempDir()}
	database := chatgptauth.Database{
		Active: "synthetic-client",
		Accounts: []chatgptauth.Auth{{
			ClientID:    "synthetic-client",
			Issuer:      chatgptauth.Issuer,
			Subject:     "synthetic-subject",
			AccessToken: "synthetic-access",
			TokenType:   "Bearer",
			Scopes:      []string{"resource.invoke", "chatgpt.tokens.use.direct"},
			ExpiresAt:   expiresAt,
		}},
	}
	contents, err := json.Marshal(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "accounts.json"), contents, 0600); err != nil {
		t.Fatal(err)
	}
	return store
}
