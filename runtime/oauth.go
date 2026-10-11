package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PKCEProof contains an RFC 7636 verifier and its S256 challenge.
type PKCEProof struct{ Verifier, Challenge string }

func NewPKCEProof() (PKCEProof, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return PKCEProof{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(verifier))
	return PKCEProof{verifier, base64.RawURLEncoding.EncodeToString(hash[:])}, nil
}

type OAuthToken struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}
type OAuthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *OAuthError) Error() string { return "OAuth " + e.Code + ": " + e.Description }

// OAuthClient uses an externally owned HTTP client and never logs tokens.
type OAuthClient struct {
	HTTPClient                    *http.Client
	TokenURL, DeviceURL, ClientID string
}

func (o OAuthClient) post(ctx context.Context, endpoint string, values url.Values, result any) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("invalid OAuth endpoint")
	}
	values.Set("client_id", o.ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := o.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		var oauthErr OAuthError
		if json.NewDecoder(resp.Body).Decode(&oauthErr) != nil || oauthErr.Code == "" {
			return fmt.Errorf("OAuth HTTP %d", resp.StatusCode)
		}
		return &oauthErr
	}
	return json.NewDecoder(resp.Body).Decode(result)
}
func (o OAuthClient) ExchangeCode(ctx context.Context, code, redirectURI, verifier string) (OAuthToken, error) {
	var token OAuthToken
	err := o.post(ctx, o.TokenURL, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {verifier}}, &token)
	if err == nil && token.AccessToken == "" {
		err = fmt.Errorf("OAuth response missing access_token")
	}
	return token, err
}
func (o OAuthClient) Refresh(ctx context.Context, refreshToken string) (OAuthToken, error) {
	var token OAuthToken
	err := o.post(ctx, o.TokenURL, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}}, &token)
	if err == nil && token.AccessToken == "" {
		err = fmt.Errorf("OAuth response missing access_token")
	}
	return token, err
}

type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

func (o OAuthClient) AuthorizeDevice(ctx context.Context, scope string) (DeviceAuthorization, error) {
	var auth DeviceAuthorization
	err := o.post(ctx, o.DeviceURL, url.Values{"scope": {scope}}, &auth)
	if err == nil && (auth.DeviceCode == "" || auth.ExpiresIn <= 0) {
		err = fmt.Errorf("invalid device authorization")
	}
	return auth, err
}
func (o OAuthClient) PollDevice(ctx context.Context, auth DeviceAuthorization) (OAuthToken, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(auth.ExpiresIn)*time.Second)
	defer cancel()
	interval := time.Duration(auth.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return OAuthToken{}, ctx.Err()
		case <-timer.C:
		}
		var token OAuthToken
		err := o.post(ctx, o.TokenURL, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {auth.DeviceCode}}, &token)
		if err == nil {
			if token.AccessToken == "" {
				return token, fmt.Errorf("OAuth response missing access_token")
			}
			return token, nil
		}
		if oauthErr, ok := err.(*OAuthError); ok {
			switch oauthErr.Code {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			}
		}
		return OAuthToken{}, err
	}
}

// TokenManager serializes refreshes, preserving rotated refresh tokens.
type TokenManager struct {
	gate    chan struct{}
	OAuth   OAuthClient
	token   OAuthToken
	expires time.Time
}

func NewTokenManager(o OAuthClient, token OAuthToken) *TokenManager {
	return &TokenManager{gate: make(chan struct{}, 1), OAuth: o, token: token, expires: time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)}
}
func (m *TokenManager) Token(ctx context.Context) (string, error) {
	if m.gate == nil {
		return "", fmt.Errorf("token manager must be created with NewTokenManager")
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case m.gate <- struct{}{}:
	}
	defer func() { <-m.gate }()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if m.token.AccessToken != "" && (m.token.ExpiresIn == 0 || time.Until(m.expires) > 30*time.Second) {
		return m.token.AccessToken, nil
	}
	if m.token.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token")
	}
	token, err := m.OAuth.Refresh(ctx, m.token.RefreshToken)
	if err != nil {
		return "", err
	}
	if token.RefreshToken == "" {
		token.RefreshToken = m.token.RefreshToken
	}
	m.token = token
	m.expires = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	return token.AccessToken, nil
}
