package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/majorcontext/ward/internal/devicelogin"
	"github.com/majorcontext/ward/internal/token"
	"github.com/majorcontext/ward/wardapi"
)

const maxTokenAnswer = 16 << 20

const tokenUsage = "WARD_URL=... ward token create --principal P [--scope land] --repo R [--repo R]... [--ttl D] | list | revoke ID"

type repoList []string

func (l *repoList) String() string     { return strings.Join(*l, ",") }
func (l *repoList) Set(v string) error { *l = append(*l, v); return nil }

// runToken is `ward token create|list|revoke`. The service accepts it only with an operator token, which WARD_TOKEN holds
// or which WARD_KEY_FILE and WARD_KID sign.
func runToken(ctx context.Context, args []string, getenv func(string) string, now func() time.Time, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: "+tokenUsage)
		return 2
	}
	if isHelp(args[0]) {
		fmt.Fprintln(stdout, "usage: "+tokenUsage)
		return 0
	}
	fs := flag.NewFlagSet("token "+args[0], flag.ContinueOnError)
	principal := fs.String("principal", "", "who the token is for, such as an email")
	scope := fs.String("scope", "land", "land")
	ttl := fs.Duration("ttl", 0, "lifetime, 0 for none")
	var repos repoList
	fs.Var(&repos, "repo", "repository text such as native:repo_X (repeatable)")
	want, known := map[string]int{"create": 0, "list": 0, "revoke": 1}[args[0]]
	if code, done := parseFlags(fs, args[1:], tokenUsage, stdout, stderr); done {
		return code
	}
	if !known || fs.NArg() != want || getenv("WARD_URL") == "" {
		fmt.Fprintln(stderr, "usage: "+tokenUsage)
		return 2
	}
	tok, err := credential(getenv, token.ScopeOperator, "", now())
	if err != nil {
		fmt.Fprintln(stderr, "ward token:", err)
		return 2
	}
	method, path, body := http.MethodGet, "/v1/tokens", []byte(nil)
	switch args[0] {
	case "create":
		method = http.MethodPost
		body, _ = json.Marshal(map[string]any{"principal": *principal, "scope": *scope, "repos": repos, "ttl_seconds": int64(ttl.Seconds())})
	case "revoke":
		method, path = http.MethodDelete, "/v1/tokens/"+url.PathEscape(fs.Arg(0))
	}
	raw, err := sendToken(ctx, method, strings.TrimRight(getenv("WARD_URL"), "/")+path, tok, body)
	if err != nil {
		fmt.Fprintln(stderr, "ward token "+args[0]+":", err)
		return 1
	}
	switch args[0] {
	case "create":
		var out struct {
			Token     string              `json:"token"`
			UserToken struct{ ID string } `json:"user_token"`
		}
		if json.Unmarshal(raw, &out) != nil || out.Token == "" {
			fmt.Fprintln(stderr, "ward token create: the answer holds no token")
			return 1
		}
		fmt.Fprintln(stdout, out.Token)
		fmt.Fprintf(stderr, "id %s: this is the only time that the token is shown\n", out.UserToken.ID)
	case "list":
		var out struct {
			Tokens []struct {
				ID, Principal, Scope string
				Repos                []string
				ExpiresAt            string `json:"expires_at"`
				RevokedAt            string `json:"revoked_at"`
			}
		}
		if json.Unmarshal(raw, &out) != nil {
			fmt.Fprintln(stderr, "ward token list: the answer is not a list")
			return 1
		}
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		for _, r := range out.Tokens {
			state := "active"
			if r.RevokedAt != "" {
				state = "revoked"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Principal, r.Scope, strings.Join(r.Repos, ","), r.ExpiresAt, state)
		}
		_ = w.Flush()
	}
	return 0
}

func sendToken(ctx context.Context, method, target, tok string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, landReadLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("WARD_URL is not a URL")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirect.Do(req)
	if err != nil {
		return nil, errors.New("the request failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenAnswer+1))
	if err != nil || len(raw) > maxTokenAnswer {
		return nil, errors.New("the answer is unreadable or too large")
	}
	if resp.StatusCode/100 != 2 {
		var env wardapi.ErrorBody
		_ = json.Unmarshal(raw, &env)
		return nil, fmt.Errorf("status %d: %s: %s", resp.StatusCode, env.Error.Code, env.Error.Message)
	}
	return raw, nil
}

// runLogin is `ward login [--print] [--scope S]`. Without --print it runs the device flow against WARD_OIDC_ISSUER with
// WARD_OIDC_CLIENT_ID and saves the token. With --print it prints the saved token.
func runLogin(ctx context.Context, args []string, getenv func(string) string, now func() time.Time, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	print := fs.Bool("print", false, "print the saved token and exit")
	scope := fs.String("scope", "", "OAuth scope, default \"openid email\"")
	const usage = "WARD_OIDC_ISSUER=... WARD_OIDC_CLIENT_ID=... ward login [--print] [--scope S]"
	if code, done := parseFlags(fs, args, usage, stdout, stderr); done {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: "+usage)
		return 2
	}
	path, err := devicelogin.DefaultPath("ward")
	if err != nil {
		fmt.Fprintln(stderr, "ward login:", err)
		return 1
	}
	if *print {
		t, err := devicelogin.Load(path, now())
		if err != nil {
			fmt.Fprintln(stderr, "ward login:", err)
			return 1
		}
		fmt.Fprintln(stdout, t.Value)
		return 0
	}
	if getenv("WARD_OIDC_ISSUER") == "" || getenv("WARD_OIDC_CLIENT_ID") == "" {
		fmt.Fprintln(stderr, "usage: "+usage)
		return 2
	}
	tok, err := devicelogin.Login(ctx, devicelogin.Config{Issuer: getenv("WARD_OIDC_ISSUER"), ClientID: getenv("WARD_OIDC_CLIENT_ID"), Scope: *scope,
		Prompt: func(uri, code, complete string) {
			if complete != "" {
				uri = complete
			}
			fmt.Fprintf(stderr, "Open %s and enter the code %s\n", uri, code)
		}})
	if err == nil {
		err = devicelogin.Save(path, tok)
	}
	if err != nil {
		fmt.Fprintln(stderr, "ward login:", err)
		return 1
	}
	fmt.Fprintln(stderr, "signed in")
	return 0
}
