// Package fakerunner is the minimal implementation of runnerproto.Runner for tests. It runs the argv on the host with no
// sandbox, so only a test may use it.
package fakerunner

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"os/exec"
	"time"

	"github.com/majorcontext/ward/runnerproto"
)

type Runner struct {
	Secret  []byte
	WardURL string
	Kid     string
	Key     ed25519.PrivateKey
}

var _ runnerproto.Runner = Runner{}

func (r Runner) ServeHTTP(w http.ResponseWriter, hr *http.Request) {
	body, _ := io.ReadAll(hr.Body)
	if !runnerproto.Verify(r.Secret, body, hr.Header.Get(runnerproto.SignatureHeader)) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	req, err := runnerproto.Decode(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	go r.run(req)
	w.WriteHeader(http.StatusAccepted)
}

func (r Runner) run(req runnerproto.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.Check.TimeoutSeconds)*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, req.Check.Run[0], req.Check.Run[1:]...).CombinedOutput()
	res := runnerproto.Result{RequestID: req.RequestID, Status: runnerproto.Pass, Summary: "ok"}
	if err != nil {
		res.Status, res.Summary = runnerproto.Fail, "failed: "+err.Error()+"\n"+string(out)
	}
	tok, err := runnerproto.MintCheckToken(r.Kid, r.Key, req.Repo, "fakerunner", time.Now().Add(time.Minute))
	if err == nil {
		_ = runnerproto.PostResult(context.Background(), http.DefaultClient, r.WardURL, tok, res)
	}
}
