package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"github.com/eddiedibs/banker_hub/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnvFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	in := map[string]string{
		"API_TOKEN":    "abc",
		"BNC_PASSWORD": `p@$$ 'w"rd' #1`,
		"CUSTOM_KEY":   "kept",
	}
	if err := writeEnvFile(path, in); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	out, err := config.ReadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range in {
		if out[k] != v {
			t.Errorf("%s = %q, want %q", k, out[k], v)
		}
	}
}

func TestReadEnvFileFormats(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("# comment\nA=1h   # trailing\nexport B=\"two\"\nC='it'\\''s'\n\n"), 0o600)
	got, err := config.ReadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["A"] != "1h" || got["B"] != "two" || got["C"] != "it's" {
		t.Errorf("got %v", got)
	}
	if m, err := config.ReadEnvFile(filepath.Join(t.TempDir(), "missing")); err != nil || len(m) != 0 {
		t.Errorf("missing file: %v %v", m, err)
	}
}

// fakeBinance answers the endpoints quickstart touches.
func fakeBinance() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/time":
			json.NewEncoder(w).Encode(map[string]int64{"serverTime": time.Now().UnixMilli()})
		case "/sapi/v1/account/apiRestrictions":
			w.Write([]byte(`{"ipRestrict":false,"enableReading":true,"enableWithdrawals":true}`))
		case "/sapi/v1/capital/deposit/hisrec":
			w.Write([]byte(`[{"id":"d1","amount":"1","coin":"USDT","network":"TRX","status":1,"insertTime":1759990000000}]`))
		case "/sapi/v1/capital/withdraw/history":
			w.Write([]byte(`[]`))
		default:
			w.Write([]byte(`{"code":"000000","data":[]}`))
		}
	}))
}

func TestQuickstartFlow(t *testing.T) {
	srv := fakeBinance()
	defer srv.Close()

	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	dbPath := filepath.Join(dir, "hub.db")
	os.WriteFile(envPath, []byte("BINANCE_BASE_URL="+srv.URL+"\n"), 0o600)

	answers := strings.Join([]string{
		"",     // listen address: default
		dbPath, // database path
		"1m",   // sync interval: too short, rejected
		"30m",  // sync interval
		"",     // lookback: default
		"y",    // sync Binance
		"key",  // API key
		"sec",  // API secret
		"",     // check now: yes
		"",     // sync BNC: default no
		"y",    // overwrite .env
		"y",    // first sync
	}, "\n") + "\n"

	var out bytes.Buffer
	p := &prompter{in: bufio.NewReader(strings.NewReader(answers)), out: &out}
	if err := run(context.Background(), p, envPath); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	o := out.String()

	for _, want := range []string{
		"must be at least 5m0s",
		"Key accepted",
		"Withdrawals are ENABLED",
		"no IP whitelist",
		"binance: 1 rows saved",
		"Backed up",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("output missing %q\n%s", want, o)
		}
	}

	env, _ := config.ReadEnvFile(envPath)
	if env["BINANCE_API_KEY"] != "key" || env["SYNC_INTERVAL"] != "30m" || len(env["API_TOKEN"]) != 64 {
		t.Errorf("env = %v", env)
	}
	if env["BINANCE_BASE_URL"] != srv.URL {
		t.Error("unknown keys must be preserved")
	}
	if env["BNC_CARD"] != "" {
		t.Error("BNC must stay disabled")
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("database not created: %v", err)
	}
}

func TestQuickstartAbortWritesNothing(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	p := &prompter{in: bufio.NewReader(strings.NewReader("\n")), out: &bytes.Buffer{}}
	if err := run(context.Background(), p, envPath); err != errAborted {
		t.Fatalf("err = %v, want errAborted", err)
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Error(".env must not exist after abort")
	}
}
