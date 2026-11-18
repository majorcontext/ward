package mirror

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/majorcontext/motte/api"
)

var (
	// ErrSignature is the error of a delivery that the secret does not sign.
	ErrSignature = errors.New("mirror: bad signature")
	// ErrBody is the error of a signed delivery that is not an event.
	ErrBody = errors.New("mirror: bad event body")
)

func subscriptionRequest(url string, secret []byte) api.CreateSubscriptionRequest {
	return api.CreateSubscriptionRequest{URL: url, Secret: string(secret)}
}

func (e *Engine) signed(body []byte, header string) bool {
	hexSum, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	mac := hmac.New(sha256.New, e.cfg.HookSecret)
	mac.Write(body)
	return hmac.Equal([]byte(hexSum), []byte(hex.EncodeToString(mac.Sum(nil))))
}

// hit pairs an event with the mirrors whose repo and patterns match it.
func (e *Engine) hit(ev api.Event) Hit {
	h := Hit{Event: ev}
	for _, en := range e.cfg.Mirrors {
		if en.Repo == ev.Repo && en.Matches(ev.Ref) {
			h.Mirrors = append(h.Mirrors, en.Name)
		}
	}
	return h
}

// Deliver handles one delivery of a ref event. It answers before any git work: it only records state.
func (e *Engine) Deliver(ctx context.Context, body []byte, signature string) error {
	if !e.signed(body, signature) {
		e.m.hooks.WithLabelValues("bad_signature").Inc()
		return ErrSignature
	}
	var ev api.Event
	if json.Unmarshal(body, &ev) != nil || ev.Partition < 0 || ev.Partition > 63 || ev.Seq <= 0 || ev.Repo == "" || ev.Ref == "" {
		e.m.hooks.WithLabelValues("bad_body").Inc()
		return ErrBody
	}
	h := e.hit(ev)
	res, err := e.store.RecordHook(ctx, h, e.clock.Now())
	if err != nil {
		e.m.hooks.WithLabelValues("error").Inc()
		return err
	}
	e.m.hooks.WithLabelValues([]string{"baseline", "in_order", "repeat", "gap"}[res]).Inc()
	if res == HookGap {
		e.request(func(w *wants) { w.catchup = true })
	}
	if res != HookRepeat {
		e.wakeMirrors(h.Mirrors)
	}
	return nil
}
