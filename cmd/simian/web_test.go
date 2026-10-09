// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/webui"
)

// A mistyped --ui-controllers fails before the controller reaches for a
// cluster.
func TestServeRefusesABadControllerList(t *testing.T) {
	cmd := newServeCmd()
	cmd.SetArgs([]string{"--ui-controllers=simian-2=simian-2.example.com"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--ui-controllers") {
		t.Fatalf("err = %v, want one naming --ui-controllers", err)
	}
}

func TestWebRefusesABadControllerList(t *testing.T) {
	cmd := newWebCmd()
	cmd.SetArgs([]string{"--addr=127.0.0.1:0", "--controllers=a=https://a.example.com/api"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--controllers") {
		t.Fatalf("err = %v, want one naming --controllers", err)
	}
}

// simian web serves the page and its list of Simians, 404s every other
// /api/ path, and stops when told to.
func TestWebServesTheStandaloneUI(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	list, err := webui.ParseControllers([]string{"a=http://localhost:18110", "b=http://127.0.0.1:18111"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveWeb(ctx, addr, list, quietLogger()) }()

	base := "http://" + addr
	var resp *http.Response
	for i := 0; i < 50; i++ {
		if resp, err = http.Get(base + "/healthz"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	read := func(path string) (int, string) {
		r, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	if code, body := read("/ui/"); code != 200 || !strings.Contains(body, "<title>Simian</title>") {
		t.Errorf("/ui/: %d %.60q", code, body)
	}
	if code, body := read("/api/controllers"); code != 200 || !strings.Contains(body, `"url":"http://127.0.0.1:18111"`) {
		t.Errorf("/api/controllers: %d %q", code, body)
	}
	if code, _ := read("/api/info"); code != http.StatusNotFound {
		t.Errorf("/api/info: %d, want 404", code)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("simian web did not stop")
	}
}
