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
	"strings"
	"time"

	"github.com/majorcontext/ward/internal/config"
	"github.com/majorcontext/ward/internal/token"
)

const (
	landCallLimit = 2 * time.Minute
	landTokenLife = 5 * time.Minute
	maxLandAnswer = 1 << 20
)

type landAnswer struct {
	Ref         string `json:"ref"`
	Generation  int64  `json:"generation"`
	Tip         string `json:"tip"`
	FastForward bool   `json:"fast_forward"`
	Commits     []struct {
		Old string `json:"old"`
		New string `json:"new"`
	} `json:"commits"`
	BranchDeleted bool `json:"branch_deleted"`
	Error         struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// landToken returns WARD_TOKEN, or a land token that it signs for repo with the key file of WARD_KEY_FILE and WARD_KID.
func landToken(getenv func(string) string, repo string, now time.Time) (string, error) {
	if tok := getenv("WARD_TOKEN"); tok != "" {
		return tok, nil
	}
	if getenv("WARD_KEY_FILE") == "" || getenv("WARD_KID") == "" {
		return "", errors.New("set WARD_TOKEN, or WARD_KEY_FILE and WARD_KID")
	}
	key, err := config.ReadPrivateKey(getenv("WARD_KEY_FILE"))
	if err != nil {
		return "", fmt.Errorf("WARD_KEY_FILE: %w", err)
	}
	return token.Signer{Kid: getenv("WARD_KID"), Key: key}.Mint(token.Claims{Scope: token.ScopeLand, Repo: repo, Subject: "ward-cli", Exp: now.Add(landTokenLife).Unix()})
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
	tok, err := landToken(getenv, repo, now())
	if err != nil {
		fmt.Fprintln(stderr, "ward land:", err)
		return 2
	}
	body, _ := json.Marshal(map[string]any{"repo": repo, "ref": *ref, "branch": branch, "delete_branch": *del})
	ctx, cancel := context.WithTimeout(ctx, landCallLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(getenv("WARD_URL"), "/")+"/v1/lands", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(stderr, "ward land: WARD_URL is not a URL")
		return 2
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "ward land: the request failed")
		return 1
	}
	defer resp.Body.Close()
	var out landAnswer
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxLandAnswer))
	if json.Unmarshal(raw, &out) != nil {
		fmt.Fprintf(stderr, "ward land: status %d with no JSON body\n", resp.StatusCode)
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		return printLandError(stderr, out, repo, *ref, resp.StatusCode)
	}
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
	return 0
}

func printLandError(stderr io.Writer, out landAnswer, repo, ref string, status int) int {
	if out.Error.Code != "land_conflict" {
		fmt.Fprintf(stderr, "ward land: status %d: %s: %s\n", status, out.Error.Code, out.Error.Message)
		return 1
	}
	fmt.Fprintf(stderr, "conflict: the branch changes paths that changed on %s of %s since it was built:\n", ref, repo)
	paths, _ := out.Error.Details["paths"].([]any)
	for _, p := range paths {
		fmt.Fprintln(stderr, p)
	}
	return 1
}
