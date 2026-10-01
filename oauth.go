package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// The tool authorizes as the same OAuth client Claude Code registers for claude.ai subscription logins,
// so each account gets its own grant that never interferes with the tokens Claude Code itself holds.
const (
	oauthClientID  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthAuthorize = "https://claude.ai/oauth/authorize"
	oauthTokenPath = "/v1/oauth/token"
	oauthScopes    = "user:profile user:inference"
	callbackPort   = 54545
	callbackPath   = "/callback"
	loginTimeout   = 5 * time.Minute
)

type tokenRequest struct {
	GrantType    string `json:"grant_type"`
	ClientID     string `json:"client_id"`
	Code         string `json:"code,omitempty"`
	RedirectURI  string `json:"redirect_uri,omitempty"`
	CodeVerifier string `json:"code_verifier,omitempty"`
	State        string `json:"state,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Account      struct {
		EmailAddress string `json:"email_address"`
	} `json:"account"`
	Organization struct {
		Name string `json:"name"`
	} `json:"organization"`
}

// grant is what the authorization server hands back through the browser.
type grant struct {
	code  string
	state string
	err   error
}

// login runs the authorization-code + PKCE flow: it opens the browser, catches the redirect on a loopback
// listener (or accepts a pasted redirect URL), then exchanges the code for tokens.
func (c *client) login(ctx context.Context, in io.Reader, out io.Writer, openURL bool) (*tokenResponse, error) {
	verifier, err := randomToken()
	if err != nil {
		return nil, err
	}
	state, err := randomToken()
	if err != nil {
		return nil, err
	}
	ln, err := listenCallback()
	if err != nil {
		return nil, fmt.Errorf("start callback listener: %w", err)
	}
	defer ln.Close()
	redirectURI := fmt.Sprintf("http://localhost:%d%s", ln.Addr().(*net.TCPAddr).Port, callbackPath)
	authURL := authorizeURL(pkceChallenge(verifier), state, redirectURI)

	fmt.Fprintf(out, "Open this URL in your browser and authorize:\n\n  %s\n\n", authURL)
	if openURL {
		openBrowser(authURL)
	}
	fmt.Fprintln(out, "Waiting for the browser to redirect back to this machine...")
	fmt.Fprintln(out, "If the browser runs elsewhere, paste the final redirect URL (or the code#state text) here and press Enter.")

	g, err := waitForGrant(ctx, ln, in, state)
	if err != nil {
		return nil, err
	}
	tok, err := c.postToken(ctx, tokenRequest{
		GrantType:    "authorization_code",
		ClientID:     oauthClientID,
		Code:         g.code,
		RedirectURI:  redirectURI,
		CodeVerifier: verifier,
		State:        g.state,
	})
	if err != nil {
		return nil, fmt.Errorf("exchange authorization code: %w", err)
	}
	return tok, nil
}

func (c *client) postToken(ctx context.Context, req tokenRequest) (*tokenResponse, error) {
	var tok tokenResponse
	if err := c.doJSON(ctx, http.MethodPost, c.oauthBase+oauthTokenPath, "", req, &tok); err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, errors.New("token endpoint returned no access token")
	}
	return &tok, nil
}

// listenCallback prefers the port Claude Code itself registers and falls back to any free port when it is busy.
func listenCallback() (net.Listener, error) {
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", callbackPort)); err == nil {
		return ln, nil
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

func authorizeURL(challenge, state, redirectURI string) string {
	params := url.Values{
		"code":                  {"true"},
		"client_id":             {oauthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {oauthScopes},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return oauthAuthorize + "?" + params.Encode()
}

// waitForGrant returns whichever arrives first: the loopback redirect or a line pasted on stdin.
func waitForGrant(ctx context.Context, ln net.Listener, in io.Reader, expectedState string) (grant, error) {
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	grants := make(chan grant, 1)
	srv := &http.Server{Handler: callbackHandler(expectedState, grants), ReadHeaderTimeout: 5 * time.Second}
	defer srv.Close()
	go srv.Serve(ln)

	lines := make(chan string, 1)
	go readLine(in, lines)

	select {
	case g := <-grants:
		if g.err != nil {
			return grant{}, g.err
		}
		return g, nil
	case line := <-lines:
		return parseGrantInput(line, expectedState)
	case <-ctx.Done():
		return grant{}, fmt.Errorf("no authorization received: %w", ctx.Err())
	}
}

func callbackHandler(expectedState string, grants chan<- grant) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != callbackPath {
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query()
		if query.Get("state") != expectedState {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		g := grant{code: query.Get("code"), state: expectedState}
		if reason := query.Get("error"); reason != "" {
			g.err = fmt.Errorf("authorization refused: %s", reason)
		} else if g.code == "" {
			g.err = errors.New("callback carried no code")
		}
		if g.err != nil {
			http.Error(w, g.err.Error(), http.StatusBadRequest)
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<!doctype html><title>claude-usage</title><p>Logged in. You can close this tab.</p>")
		}
		// Only the first callback matters; a browser retry must never block the handler.
		select {
		case grants <- g:
		default:
		}
	})
}

// readLine forwards the first non-empty stdin line; a closed stdin simply never fires.
func readLine(in io.Reader, lines chan<- string) {
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			lines <- line
			return
		}
	}
}

// parseGrantInput accepts the full redirect URL, the "code#state" text claude.ai shows, or a bare code.
func parseGrantInput(input, expectedState string) (grant, error) {
	g := grant{state: expectedState}
	switch {
	case strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://"):
		parsed, err := url.Parse(input)
		if err != nil {
			return grant{}, fmt.Errorf("parse redirect URL: %w", err)
		}
		g.code = parsed.Query().Get("code")
		if state := parsed.Query().Get("state"); state != "" {
			g.state = state
		}
	case strings.Contains(input, "#"):
		g.code, g.state, _ = strings.Cut(input, "#")
	default:
		g.code = input
	}
	if g.code == "" {
		return grant{}, errors.New("no authorization code in input")
	}
	if g.state != expectedState {
		return grant{}, errors.New("state mismatch in pasted input")
	}
	return g, nil
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// openBrowser is best effort: the URL is always printed, so a failure here only costs the user a copy-paste.
func openBrowser(target string) {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	_ = cmd.Start()
}
