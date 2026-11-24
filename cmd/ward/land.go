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
	"strconv"
	"strings"
	"time"

	"github.com/majorcontext/ward/internal/config"
	"github.com/majorcontext/ward/internal/devicelogin"
	"github.com/majorcontext/ward/internal/token"
)

const (
	landTokenLife = time.Hour
	landWaitSecs  = 60
	maxLandAnswer = 1 << 20
	// landReadLimit bounds one read of a land: the wait, and a margin for the answer.
	landReadLimit = (landWaitSecs + 15) * time.Second
	// landRetryWindow bounds how long a poll may fail in a row before the command gives up.
	landRetryWindow = 2 * time.Minute
	landRetryFirst  = 500 * time.Millisecond
	landRetryMax    = 10 * time.Second
	// landRetryLast is the time that the last read of the window gets.
	landRetryLast = time.Second
)

type landAnswer struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Ref    string `json:"ref"`
	Checks []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"checks"`
	Tip         string `json:"tip"`
	Generation  int64  `json:"generation"`
	FastForward bool   `json:"fast_forward"`
	Commits     []struct {
		Old string `json:"old"`
		New string `json:"new"`
	} `json:"commits"`
	BranchDeleted bool       `json:"branch_deleted"`
	Error         *landError `json:"error"`
}

type landError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

type landEnvelope struct {
	Error landError `json:"error"`
}

// credential returns WARD_TOKEN, or a token of scope for repo that it signs with the key file of WARD_KEY_FILE and WARD_KID, or
// the token that `ward login` saved. It returns no token when only WARD_ACCESS_TOKEN is set: the proxy then proves the identity.
func credential(getenv func(string) string, scope token.Scope, repo string, now time.Time) (string, error) {
	if tok := getenv("WARD_TOKEN"); tok != "" {
		return tok, nil
	}
	if getenv("WARD_KEY_FILE") != "" && getenv("WARD_KID") != "" {
		key, err := config.ReadPrivateKey(getenv("WARD_KEY_FILE"))
		if err != nil {
			return "", fmt.Errorf("WARD_KEY_FILE: %w", err)
		}
		return token.Signer{Kid: getenv("WARD_KID"), Key: key}.Mint(token.Claims{Scope: scope, Repo: repo, Subject: "ward-cli", Exp: now.Add(landTokenLife).Unix()})
	}
	if path, err := devicelogin.DefaultPath("ward"); err == nil {
		if t, err := devicelogin.Load(path, now); err == nil {
			return t.Value, nil
		}
	}
	if getenv("WARD_ACCESS_TOKEN") != "" {
		return "", nil
	}
	return "", errors.New("set WARD_TOKEN, or WARD_KEY_FILE and WARD_KID, or WARD_ACCESS_TOKEN, or run ward login")
}

// accessHeader returns the header and value of WARD_ACCESS_TOKEN, the credential of an identity-aware proxy in front of ward.
// WARD_ACCESS_HEADER names the header. The default cf-access-token is the one that Cloudflare Access reads.
func accessHeader(getenv func(string) string) (name, value string) {
	name = getenv("WARD_ACCESS_HEADER")
	if name == "" {
		name = "cf-access-token"
	}
	return name, getenv("WARD_ACCESS_TOKEN")
}

// runLand is `ward land [--ref REF] [--delete-branch] REPO BRANCH`. It returns the exit code: 0 landed, 1 refused or failed, 2 usage.
func runLand(ctx context.Context, args []string, getenv func(string) string, now func() time.Time, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("land", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ref := fs.String("ref", "main", "ref to land on")
	del := fs.Bool("delete-branch", false, "delete the branch after it landed")
	if fs.Parse(args) != nil || fs.NArg() != 2 || getenv("WARD_URL") == "" {
		fmt.Fprintln(stderr, "usage: WARD_URL=... ward land [--ref REF] [--delete-branch] REPO BRANCH")
		return 2
	}
	repo, branch := fs.Arg(0), fs.Arg(1)
	tok, err := credential(getenv, token.ScopeLand, repo, now())
	if err != nil {
		fmt.Fprintln(stderr, "ward land:", err)
		return 2
	}
	c := &landClient{base: strings.TrimRight(getenv("WARD_URL"), "/"), tok: tok}
	c.accessName, c.accessValue = accessHeader(getenv)
	body, _ := json.Marshal(map[string]any{"repo": repo, "ref": *ref, "branch": branch, "delete_branch": *del})
	cur, code := c.call(ctx, http.MethodPost, "/v1/lands", body, stderr)
	if code != 0 {
		return code
	}
	announced := false
	for {
		if !announced && cur.State == "checking" {
			announced = true
			for _, ck := range cur.Checks {
				if ck.State != "pass" && ck.State != "flaky" {
					fmt.Fprintln(stdout, "checking", ck.Name)
				}
			}
		}
		switch cur.State {
		case "landed":
			printLanded(stdout, cur, branch, repo)
			return 0
		case "failed":
			return printLandError(stderr, cur.Error)
		}
		path := "/v1/lands/" + cur.ID + "?wait=" + strconv.Itoa(landWaitSecs) + "&state=" + cur.State
		if cur, code = c.poll(ctx, cur.ID, path, stderr); code != 0 {
			return code
		}
	}
}

// poll reads a land that the server has accepted. The land goes on without the client, so a transport error or a 5xx answer
// is tried again with a growing pause until landRetryWindow passes without one good answer.
func (c *landClient) poll(ctx context.Context, id, path string, stderr io.Writer) (landAnswer, int) {
	var deadline time.Time
	pause := landRetryFirst
	for {
		limit := landReadLimit
		if !deadline.IsZero() {
			limit = min(limit, time.Until(deadline))
		}
		out, fail := c.send(ctx, http.MethodGet, path, nil, limit)
		if fail == nil {
			return out, 0
		}
		if !fail.transient {
			fmt.Fprintln(stderr, "ward land:", fail.msg)
			return landAnswer{}, fail.exit
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(landRetryWindow)
		}
		if ctx.Err() != nil || time.Until(deadline) <= landRetryLast {
			fmt.Fprintf(stderr, "ward land: lost contact with the server (%s); land %s may still finish: read GET /v1/lands/%s on WARD_URL to see its state\n", fail.msg, id, id)
			return landAnswer{}, 1
		}
		fmt.Fprintf(stderr, "ward land: %s; reading land %s again\n", fail.msg, id)
		select {
		case <-time.After(min(pause, time.Until(deadline)-landRetryLast)):
		case <-ctx.Done():
		}
		pause = min(pause*2, landRetryMax)
	}
}

// noRedirect never follows a redirect, so a credential header never reaches another origin.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// landClient sends its token as a bearer token, and the access token in its own header.
type landClient struct{ base, tok, accessName, accessValue string }

// callFailure is a failed request. A transient one may succeed when repeated.
type callFailure struct {
	exit      int
	msg       string
	transient bool
}

// call sends one request and decodes the land of the answer. A non-zero code is the exit code of a failure that it has reported.
// A POST goes through call and is never repeated: each POST makes a new land.
func (c *landClient) call(ctx context.Context, method, path string, body []byte, stderr io.Writer) (landAnswer, int) {
	out, fail := c.send(ctx, method, path, body, landReadLimit)
	if fail != nil {
		fmt.Fprintln(stderr, "ward land:", fail.msg)
		return landAnswer{}, fail.exit
	}
	return out, 0
}

func (c *landClient) send(ctx context.Context, method, path string, body []byte, limit time.Duration) (landAnswer, *callFailure) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return landAnswer{}, &callFailure{exit: 2, msg: "WARD_URL is not a URL"}
	}
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	if c.accessValue != "" {
		req.Header.Set(c.accessName, c.accessValue)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirect.Do(req)
	if err != nil {
		return landAnswer{}, &callFailure{exit: 1, msg: "the request failed", transient: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxLandAnswer))
	ok := resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted
	if err != nil && (ok || resp.StatusCode >= 500) {
		return landAnswer{}, &callFailure{exit: 1, msg: "the answer was cut off", transient: true}
	}
	if !ok && resp.StatusCode != http.StatusAccepted {
		fail := &callFailure{exit: 1, transient: resp.StatusCode >= 500}
		var env landEnvelope
		if err != nil || json.Unmarshal(raw, &env) != nil {
			fail.msg = fmt.Sprintf("status %d with no JSON body", resp.StatusCode)
		} else {
			fail.msg = fmt.Sprintf("status %d: %s: %s", resp.StatusCode, env.Error.Code, env.Error.Message)
		}
		return landAnswer{}, fail
	}
	var out landAnswer
	if json.Unmarshal(raw, &out) != nil || out.ID == "" {
		return landAnswer{}, &callFailure{exit: 1, msg: fmt.Sprintf("status %d with no land", resp.StatusCode)}
	}
	return out, nil
}

func printLanded(stdout io.Writer, out landAnswer, branch, repo string) {
	fmt.Fprintf(stdout, "landed %s on %s %s\ntip %s\ngeneration %d\n", branch, repo, out.Ref, out.Tip, out.Generation)
	if out.FastForward {
		fmt.Fprintln(stdout, "fast-forward")
	}
	for _, c := range out.Commits {
		fmt.Fprintf(stdout, "%s -> %s\n", c.Old, c.New)
	}
	if out.BranchDeleted {
		fmt.Fprintln(stdout, "branch deleted")
	}
}

func printLandError(stderr io.Writer, e *landError) int {
	if e == nil {
		fmt.Fprintln(stderr, "ward land: the land failed")
		return 1
	}
	fmt.Fprintf(stderr, "ward land: %s: %s\n", e.Code, e.Message)
	switch e.Code {
	case "land_conflict":
		paths, _ := e.Details["paths"].([]any)
		for _, p := range paths {
			fmt.Fprintln(stderr, p)
		}
	case "check_failed":
		for _, k := range []string{"check", "summary", "log_url"} {
			fmt.Fprintf(stderr, "%s: %v\n", k, e.Details[k])
		}
	}
	return 1
}
