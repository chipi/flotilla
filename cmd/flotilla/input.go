package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/input"
)

const inputUsage = `input show KEY [--ledger] | input pin KEY | input unpin KEY | input export KEY --to DIR [--with-ledger] [--with-state]  (an operator's: a frozen input is production data)`

// inputCmd is `input`: the frozen input of a live run (docs/wip/v2-design.md sections 4 and 6), which a shadow of the routine is
// given. show reads it, pin keeps its state snapshot past its shadows, export writes it to a folder outside any repository.
func inputCmd(args []string) {
	dir := flagValue(&args, "--to")
	showLedger, withLedger, withState := flagBool(&args, "--ledger"), flagBool(&args, "--with-ledger"), flagBool(&args, "--with-state")
	need(args, 2, inputUsage)
	sub, key := args[0], args[1]
	switch sub {
	case "show":
		path := "/v1/inputs/" + url.PathEscape(key)
		if showLedger {
			path += "?ledger=1"
		}
		var out api.InputView
		get(path, &out)
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	case "pin", "unpin":
		post("/v1/inputs/"+url.PathEscape(key)+"/"+sub, nil)
	case "export":
		if dir == "" {
			fail(fmt.Errorf("usage: flotilla %s", inputUsage))
		}
		if err := exportInput(&http.Client{}, base(), token(), key, dir, withLedger, withState, time.Now()); err != nil {
			fail(err)
		}
	default:
		fail(fmt.Errorf("usage: flotilla %s", inputUsage))
	}
}

// exportInput writes an input to a new folder: input.json (the envelope, with the ledger rows as of its mark with withLedger),
// state.tar.gz (the routine's state snapshot, with withState: it must still be held, so pin before its shadows end) and a README
// that says it is production data. The folder is checked before anything is fetched (never inside a git work tree, never one that
// holds something), is made 0700 with its files 0600, and a failed export leaves nothing behind.
func exportInput(c *http.Client, baseURL, tok, key, dir string, withLedger, withState bool, now time.Time) error {
	if _, err := input.CheckFolder(dir); err != nil {
		return err
	}
	path := "/v1/inputs/" + url.PathEscape(key)
	body, status, err := fetch(c, baseURL+path+map[bool]string{true: "?ledger=1", false: ""}[withLedger], tok)
	if err != nil {
		return err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 1<<30))
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", status, bytes.TrimSpace(raw))
	}
	var view api.InputView
	if err := json.Unmarshal(raw, &view); err != nil {
		return fmt.Errorf("the input: %w", err)
	}
	var state io.Reader
	if withState {
		if view.Envelope.StateSnapshot == nil {
			return fmt.Errorf("input %s has no state snapshot: its routine declares no state paths", key)
		}
		sb, sstatus, err := fetch(c, baseURL+path+"/state", tok)
		if err != nil {
			return err
		}
		defer sb.Close()
		if sstatus/100 != 2 {
			msg, _ := io.ReadAll(io.LimitReader(sb, 1<<16))
			return fmt.Errorf("HTTP %d: %s", sstatus, bytes.TrimSpace(msg))
		}
		state = sb
	}
	if err := input.WriteFolder(dir, view.Envelope, state, now); err != nil {
		return err
	}
	where, _ := filepath.Abs(dir)
	fmt.Printf("exported input %s to %s (production data: keep it outside any repository)\n", key, where)
	return nil
}

// fetch is a GET with the operator's token and no overall deadline (a state archive can be large); the caller closes the body.
func fetch(c *http.Client, u, tok string) (io.ReadCloser, int, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.StatusCode, nil
}
