package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

func TestHandleNotification(t *testing.T) {
	h := newTestHandler(fakeReader{}, &fakeController{})
	// No "id" => a notification: no response is written.
	raw, isNote := h.handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if !isNote {
		t.Fatal("message without id should be treated as a notification")
	}
	if raw != nil {
		t.Fatalf("notification should produce no response, got %s", raw)
	}
}

func TestHandleParseError(t *testing.T) {
	h := newTestHandler(fakeReader{}, &fakeController{})
	raw, isNote := h.handle(context.Background(), []byte(`{not json`))
	if isNote {
		t.Fatal("a parse error is a response, not a notification")
	}
	var out struct {
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Error == nil || out.Error.Code != codeParse {
		t.Fatalf("want parse error %d, got %+v", codeParse, out.Error)
	}
}

func TestInitialize(t *testing.T) {
	h := newTestHandler(fakeReader{}, &fakeController{})
	raw, _ := h.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	var out struct {
		Result struct {
			Capabilities map[string]json.RawMessage `json:"capabilities"`
			ServerInfo   struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out.Result.Capabilities["tools"]; !ok {
		t.Fatalf("initialize must advertise the tools capability: %s", raw)
	}
	if out.Result.ServerInfo.Name == "" {
		t.Fatalf("initialize must report serverInfo.name: %s", raw)
	}
}

func TestToolsList(t *testing.T) {
	h := newTestHandler(fakeReader{}, &fakeController{})
	raw, _ := h.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	var out struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"list_tasks": true, "get_task": true, "get_audit": true, "cancel_task": true, "enqueue_task": true}
	if len(out.Result.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(out.Result.Tools), len(want))
	}
	for _, tl := range out.Result.Tools {
		if !want[tl.Name] {
			t.Fatalf("unexpected tool %q", tl.Name)
		}
		if len(tl.InputSchema) == 0 {
			t.Fatalf("tool %q missing inputSchema", tl.Name)
		}
	}
}

func TestUnknownMethod(t *testing.T) {
	h := newTestHandler(fakeReader{}, &fakeController{})
	raw, _ := h.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"no/such"}`))
	var out struct {
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Error == nil || out.Error.Code != codeMethodNotFn {
		t.Fatalf("want method-not-found %d, got %+v", codeMethodNotFn, out.Error)
	}
}
