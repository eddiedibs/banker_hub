// Command quickstart is an interactive first-time setup for bankerhub.
// It asks for the server settings and credentials, checks the
// credentials against the live services, writes .env and creates the
// database.
//
//	go run ./cmd/quickstart [-env .env]
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"github.com/eddiedibs/banker_hub/internal/config"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/eddiedibs/banker_hub/internal/provider"
	"github.com/eddiedibs/banker_hub/internal/provider/binance"
	"github.com/eddiedibs/banker_hub/internal/provider/bnc"
	"github.com/eddiedibs/banker_hub/internal/store"
	"github.com/eddiedibs/banker_hub/internal/syncer"
)

const totalSteps = 5

func main() {
	envPath := flag.String("env", ".env", "path of the env file to create or update")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	p := newTerminalPrompter()
	if err := run(ctx, p, *envPath); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, errAborted) {
			fmt.Fprintln(p.out, "\nSetup cancelled. Nothing was written.")
			os.Exit(130)
		}
		p.fail(err.Error())
		os.Exit(1)
	}
}

// setup collects the answers before anything is written.
type setup struct {
	env      map[string]string
	firstRun bool
	binance  bool
	bnc      bool
}

func run(ctx context.Context, p *prompter, envPath string) error {
	existing, err := config.ReadEnvFile(envPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", envPath, err)
	}

	fmt.Fprintf(p.out, "%sbanker_hub quickstart%s\n", p.c.bold, p.c.reset)
	if len(existing) > 0 {
		p.note(fmt.Sprintf("Found %s. Current values are the defaults; press Enter to keep them.", envPath))
	} else {
		p.note("Press Enter to accept the value in [brackets].")
	}

	s := &setup{env: map[string]string{}, firstRun: len(existing) == 0}
	for k, v := range existing {
		s.env[k] = v
	}

	if err := askServer(p, s); err != nil {
		return err
	}
	if err := askBinance(ctx, p, s); err != nil {
		return err
	}
	if err := askBNC(ctx, p, s); err != nil {
		return err
	}

	p.step(4, totalSteps, "PayPal")
	p.note("PayPal is a placeholder in this version. Nothing to configure.")

	return finish(ctx, p, s, envPath, len(existing) > 0)
}

func askServer(p *prompter, s *setup) error {
	p.step(1, totalSteps, "Server")
	var err error
	if s.env["HTTP_ADDR"], err = p.ask("Listen address", or(s.env["HTTP_ADDR"], "127.0.0.1:8080"), validateAddr); err != nil {
		return err
	}
	if host, _, _ := net.SplitHostPort(s.env["HTTP_ADDR"]); host != "127.0.0.1" && host != "localhost" && host != "::1" {
		p.warn("The API is reachable from other machines. Put a TLS reverse proxy in front of it.")
	}
	if s.env["DB_PATH"], err = p.ask("SQLite database path", or(s.env["DB_PATH"], "banker_hub.db"), nonEmpty); err != nil {
		return err
	}
	if s.env["SYNC_INTERVAL"], err = p.ask("Sync every", or(s.env["SYNC_INTERVAL"], "2h"), minDuration(5*time.Minute)); err != nil {
		return err
	}
	if s.env["SYNC_LOOKBACK"], err = p.ask("First sync reaches back", or(s.env["SYNC_LOOKBACK"], "2160h"), minDuration(time.Hour)); err != nil {
		return err
	}
	s.env["LOG_LEVEL"] = or(s.env["LOG_LEVEL"], "info")

	if s.env["API_TOKEN"] != "" {
		regen, err := p.confirm("Generate a new API token?", false)
		if err != nil {
			return err
		}
		if !regen {
			p.ok("Keeping the existing API token")
			return nil
		}
	}
	s.env["API_TOKEN"] = randomToken()
	p.ok("Generated a new API token (shown at the end)")
	return nil
}

func askBinance(ctx context.Context, p *prompter, s *setup) error {
	p.step(2, totalSteps, "Binance")
	p.note("Create a key at binance.com > API Management with only \"Enable Reading\" and an IP whitelist.")
	enable, err := p.confirm("Sync Binance?", s.firstRun || s.env["BINANCE_API_KEY"] != "")
	if err != nil {
		return err
	}
	if !enable {
		s.env["BINANCE_API_KEY"], s.env["BINANCE_API_SECRET"] = "", ""
		p.note("Binance disabled.")
		return nil
	}

	for {
		if s.env["BINANCE_API_KEY"], err = p.secret("API key", s.env["BINANCE_API_KEY"]); err != nil {
			return err
		}
		if s.env["BINANCE_API_SECRET"], err = p.secret("API secret", s.env["BINANCE_API_SECRET"]); err != nil {
			return err
		}
		test, err := p.confirm("Check the key against Binance now?", true)
		if err != nil {
			return err
		}
		if !test {
			s.binance = true
			return nil
		}

		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		r, checkErr := binance.New(baseURL(s.env, "BINANCE_BASE_URL", "https://api.binance.com"),
			s.env["BINANCE_API_KEY"], s.env["BINANCE_API_SECRET"]).CheckKey(cctx)
		cancel()
		if checkErr == nil {
			p.ok("Key accepted")
			reportRestrictions(p, r)
			s.binance = true
			return nil
		}
		p.fail("Binance rejected the key: " + checkErr.Error())
		retry, err := p.confirm("Enter the key again?", true)
		if err != nil {
			return err
		}
		if !retry {
			keep, err := p.confirm("Save the key anyway?", false)
			if err != nil {
				return err
			}
			if !keep {
				s.env["BINANCE_API_KEY"], s.env["BINANCE_API_SECRET"] = "", ""
			}
			s.binance = keep
			return nil
		}
		// Clear so the prompt does not offer the rejected key as default.
		s.env["BINANCE_API_KEY"], s.env["BINANCE_API_SECRET"] = "", ""
	}
}

func reportRestrictions(p *prompter, r binance.Restrictions) {
	if !r.EnableReading {
		p.warn("Reading is not enabled on this key; syncs will fail.")
	}
	if r.EnableWithdrawals {
		p.warn("Withdrawals are ENABLED on this key. Create a read-only key instead.")
	}
	if r.EnableSpotAndMarginTrading || r.EnableMargin || r.EnableFutures || r.EnableVanillaOptions {
		p.warn("Trading permissions are enabled. bankerhub only needs reading.")
	}
	if r.EnableInternalTransfer || r.PermitsUniversalTransfer {
		p.warn("Transfer permissions are enabled. bankerhub only needs reading.")
	}
	if !r.IPRestrict {
		p.warn("The key has no IP whitelist. Restrict it to this server's IP.")
	}
	if r.EnableReading && !r.EnableWithdrawals && !r.EnableSpotAndMarginTrading && r.IPRestrict {
		p.ok("Key is read-only and IP-restricted")
	}
}

func askBNC(ctx context.Context, p *prompter, s *setup) error {
	p.step(3, totalSteps, "BNC (Banco Nacional de Crédito)")
	p.warn("BNC has no public API. bankerhub logs in to online banking like a browser.")
	p.warn("This is likely against BNC's terms and can get your access locked. Use it only for your own account.")
	enable, err := p.confirm("Sync BNC?", s.env["BNC_CARD"] != "")
	if err != nil {
		return err
	}
	if !enable {
		s.env["BNC_CARD"], s.env["BNC_USER_ID"], s.env["BNC_PASSWORD"] = "", "", ""
		p.note("BNC disabled.")
		return nil
	}

	for {
		if s.env["BNC_CARD"], err = p.secret("Card number", s.env["BNC_CARD"]); err != nil {
			return err
		}
		if s.env["BNC_USER_ID"], err = p.secret("Cédula (user ID)", s.env["BNC_USER_ID"]); err != nil {
			return err
		}
		if s.env["BNC_PASSWORD"], err = p.secret("Password", s.env["BNC_PASSWORD"]); err != nil {
			return err
		}
		p.note("A test login ends any BNC session you have open in a browser or the app.")
		test, err := p.confirm("Test the login now?", true)
		if err != nil {
			return err
		}
		if !test {
			s.bnc = true
			return nil
		}

		fmt.Fprintf(p.out, "  Logging in to BNC...")
		cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		checkErr := bnc.New(baseURL(s.env, "BNC_BASE_URL", "https://personas.bncenlinea.com"), bnc.Credentials{
			Card: s.env["BNC_CARD"], UserID: s.env["BNC_USER_ID"], Password: s.env["BNC_PASSWORD"],
		}).CheckLogin(cctx)
		cancel()
		fmt.Fprintln(p.out)
		if checkErr == nil {
			p.ok("Login works (session closed again)")
			s.bnc = true
			return nil
		}
		p.fail(checkErr.Error())
		if errors.Is(checkErr, bnc.ErrLogin) {
			p.note("Several failed logins can block your BNC user. Double-check before retrying.")
		}
		retry, err := p.confirm("Enter the credentials again?", false)
		if err != nil {
			return err
		}
		if !retry {
			keep, err := p.confirm("Save the credentials anyway?", false)
			if err != nil {
				return err
			}
			if !keep {
				s.env["BNC_CARD"], s.env["BNC_USER_ID"], s.env["BNC_PASSWORD"] = "", "", ""
			}
			s.bnc = keep
			return nil
		}
		s.env["BNC_PASSWORD"] = ""
	}
}

func finish(ctx context.Context, p *prompter, s *setup, envPath string, existed bool) error {
	p.step(5, totalSteps, "Save")
	fmt.Fprintf(p.out, "  Server    %s, database %s, sync every %s\n", s.env["HTTP_ADDR"], s.env["DB_PATH"], s.env["SYNC_INTERVAL"])
	fmt.Fprintf(p.out, "  Binance   %s\n", onOff(s.binance))
	fmt.Fprintf(p.out, "  BNC       %s\n", onOff(s.bnc))
	fmt.Fprintf(p.out, "  PayPal    placeholder\n")

	label := "Write " + envPath + "?"
	if existed {
		label = "Overwrite " + envPath + "? (a backup is kept)"
	}
	save, err := p.confirm(label, true)
	if err != nil {
		return err
	}
	if !save {
		return errAborted
	}

	if existed {
		backup := fmt.Sprintf("%s.bak-%s", envPath, time.Now().Format("20060102-150405"))
		if err := copyFile(envPath, backup); err != nil {
			return fmt.Errorf("backup %s: %w", envPath, err)
		}
		p.ok("Backed up the old file to " + backup)
	}
	if err := writeEnvFile(envPath, s.env); err != nil {
		return fmt.Errorf("write %s: %w", envPath, err)
	}
	p.ok("Wrote " + envPath + " (mode 0600)")

	st, err := store.Open(ctx, s.env["DB_PATH"])
	if err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	defer st.Close()
	p.ok("Database ready at " + s.env["DB_PATH"])

	if s.binance || s.bnc {
		// The file is already written, so a closed input only skips the sync.
		run, err := p.confirm("Run the first sync now?", true)
		if err != nil && !errors.Is(err, errAborted) {
			return err
		}
		if run {
			firstSync(ctx, p, s, st)
		}
	}

	fmt.Fprintf(p.out, "\n%sDone.%s Start the server with:\n\n    make run\n\n", p.c.bold, p.c.reset)
	fmt.Fprintf(p.out, "Your API token (also in %s):\n\n    %s\n\n", envPath, s.env["API_TOKEN"])
	fmt.Fprintf(p.out, "Try it:\n\n    curl -H \"Authorization: Bearer %s\" http://%s/transactions\n\n",
		s.env["API_TOKEN"], s.env["HTTP_ADDR"])
	return nil
}

func firstSync(ctx context.Context, p *prompter, s *setup, st *store.Store) {
	lookback, _ := time.ParseDuration(s.env["SYNC_LOOKBACK"])
	var providers []provider.Provider
	if s.binance {
		providers = append(providers, binance.New(baseURL(s.env, "BINANCE_BASE_URL", "https://api.binance.com"),
			s.env["BINANCE_API_KEY"], s.env["BINANCE_API_SECRET"]))
	}
	if s.bnc {
		providers = append(providers, bnc.New(baseURL(s.env, "BNC_BASE_URL", "https://personas.bncenlinea.com"), bnc.Credentials{
			Card: s.env["BNC_CARD"], UserID: s.env["BNC_USER_ID"], Password: s.env["BNC_PASSWORD"],
		}))
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	sy := syncer.New(st, lookback, quiet, providers...)

	fmt.Fprintf(p.out, "  Syncing (this can take a minute)...\n")
	for _, r := range sy.RunAll(ctx, provider.Range{}) {
		if r.Error != "" {
			p.fail(fmt.Sprintf("%s: %s (%d rows saved)", r.Provider, r.Error, r.Rows))
		} else {
			p.ok(fmt.Sprintf("%s: %d rows saved", r.Provider, r.Rows))
		}
	}
}

func validateAddr(v string) error {
	if _, _, err := net.SplitHostPort(v); err != nil {
		return errors.New("use host:port, for example 127.0.0.1:8080")
	}
	return nil
}

func nonEmpty(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("value is required")
	}
	return nil
}

func minDuration(min time.Duration) func(string) error {
	return func(v string) error {
		d, err := time.ParseDuration(v)
		if err != nil {
			return errors.New("use a Go duration such as 30m, 2h or 2160h")
		}
		if d < min {
			return fmt.Errorf("must be at least %s", min)
		}
		return nil
	}
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func baseURL(env map[string]string, key, def string) string { return or(env[key], def) }

func or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func onOff(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
