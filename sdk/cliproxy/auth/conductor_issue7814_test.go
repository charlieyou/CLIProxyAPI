package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type issue7814RefreshExecutor struct {
	provider  string
	refreshFn func(ctx context.Context, auth *Auth) (*Auth, error)
}

func (e *issue7814RefreshExecutor) Identifier() string { return e.provider }
func (e *issue7814RefreshExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *issue7814RefreshExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (e *issue7814RefreshExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	if e.refreshFn != nil {
		return e.refreshFn(ctx, auth)
	}
	return auth, nil
}
func (e *issue7814RefreshExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *issue7814RefreshExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManager_Issue7814_StaleInFlightSiblingSuccessDoesNotClearCredentialRetryAfter(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)
	cred := &Auth{ID: "auth-issue7814-stale-seq", Provider: "antigravity", Status: StatusActive}
	if _, errReg := mgr.Register(WithSkipPersist(ctx), cred); errReg != nil {
		t.Fatalf("Register() error = %v", errReg)
	}

	inFlightSeq := mgr.nextExecutionSeq()
	mgr.MarkResult(ctx, Result{
		AuthID:          cred.ID,
		Provider:        "antigravity",
		Model:           "gemini-3-flash",
		Success:         false,
		CredentialScope: true,
		Error:           &Error{HTTPStatus: http.StatusForbidden, Message: "VALIDATION_REQUIRED"},
	})

	// Stale in-flight sibling success dispatched before the 403 must not clear credentialRetryAfter.
	mgr.MarkResult(ctx, Result{
		AuthID:            cred.ID,
		Provider:          "antigravity",
		Model:             "claude-sonnet-4-6",
		Success:           true,
		CompletedResponse: true,
		ExecutionSeq:      inFlightSeq,
	})

	snap, _ := mgr.GetByID(cred.ID)
	if IsAuthAvailableForModel(snap, "claude-sonnet-4-6", time.Now()) {
		t.Fatalf("stale sibling success cleared credentialRetryAfter: %+v", snap)
	}
}

func TestManager_Issue7814_ValidationRequiredSurvivesSameAccountTokenRotation(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	ctx := context.Background()
	validationBody := `{"error":{"code":403,"message":"Verify your account","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED","domain":"cloudaicompanion.googleapis.com"}]}}`

	t.Run("same-account OAuth token rotation does not discard 403 VALIDATION_REQUIRED", func(t *testing.T) {
		mgr := NewManager(nil, &RoundRobinSelector{}, nil)
		cred := &Auth{
			ID:       "auth-issue7814-rotate-same",
			Provider: "antigravity",
			Status:   StatusActive,
			Metadata: map[string]any{
				"access_token":  "tok-v1",
				"refresh_token": "rt-same",
				"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
			},
		}
		registered, errReg := mgr.Register(WithSkipPersist(ctx), cred)
		if errReg != nil {
			t.Fatalf("Register() error = %v", errReg)
		}

		// Simulate OAuth access-token refresh rotating access_token (CredentialVersion 1 -> 2) with same refresh_token.
		rotated := registered.Clone()
		rotated.Metadata["access_token"] = "tok-v2"
		if _, errUpd := mgr.Update(WithSkipPersist(ctx), rotated); errUpd != nil {
			t.Fatalf("Update() error = %v", errUpd)
		}

		// In-flight request started before rotation completes with 403 VALIDATION_REQUIRED.
		mgr.MarkResult(ctx, Result{
			AuthID:            cred.ID,
			Provider:          "antigravity",
			Model:             "gemini-3-flash",
			Success:           false,
			CredentialScope:   true,
			AccessToken:       "tok-v1",
			RefreshToken:      "rt-same",
			CredentialVersion: registered.CredentialVersion,
			RegistrationEpoch: registered.RegistrationEpoch,
			Error:             &Error{HTTPStatus: http.StatusForbidden, Message: validationBody},
		})

		snap, _ := mgr.GetByID(cred.ID)
		if IsAuthAvailableForModel(snap, "gemini-3-flash", time.Now()) {
			t.Fatalf("expected gemini-3-flash blocked after 403 VALIDATION_REQUIRED across same-account token rotation: %+v", snap)
		}
		if IsAuthAvailableForModel(snap, "claude-sonnet-4-6", time.Now()) {
			t.Fatalf("expected sibling claude-sonnet-4-6 blocked after 403 VALIDATION_REQUIRED across same-account token rotation: %+v", snap)
		}
	})

	t.Run("old-token 401 is still discarded after same-account token rotation", func(t *testing.T) {
		mgr := NewManager(nil, &RoundRobinSelector{}, nil)
		cred := &Auth{
			ID:       "auth-issue7814-rotate-401-stale",
			Provider: "antigravity",
			Status:   StatusActive,
			Metadata: map[string]any{
				"access_token":  "tok-v1",
				"refresh_token": "rt-same",
				"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
			},
		}
		registered, errReg := mgr.Register(WithSkipPersist(ctx), cred)
		if errReg != nil {
			t.Fatalf("Register() error = %v", errReg)
		}

		rotated := registered.Clone()
		rotated.Metadata["access_token"] = "tok-v2"
		if _, errUpd := mgr.Update(WithSkipPersist(ctx), rotated); errUpd != nil {
			t.Fatalf("Update() error = %v", errUpd)
		}

		mgr.MarkResult(ctx, Result{
			AuthID:            cred.ID,
			Provider:          "antigravity",
			Model:             "gemini-3-flash",
			Success:           false,
			CredentialScope:   true,
			AccessToken:       "tok-v1",
			RefreshToken:      "rt-same",
			CredentialVersion: registered.CredentialVersion,
			RegistrationEpoch: registered.RegistrationEpoch,
			Error:             &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
		})

		snap, _ := mgr.GetByID(cred.ID)
		if !IsAuthAvailableForModel(snap, "gemini-3-flash", time.Now()) || !IsAuthAvailableForModel(snap, "claude-sonnet-4-6", time.Now()) {
			t.Fatalf("expected stale old-token 401 to be discarded after token rotation: %+v", snap)
		}
	})

	t.Run("credential replacement with different account still discards old 403 VALIDATION_REQUIRED", func(t *testing.T) {
		mgr := NewManager(nil, &RoundRobinSelector{}, nil)
		cred := &Auth{
			ID:       "auth-issue7814-rotate-diff-acct",
			Provider: "antigravity",
			Status:   StatusActive,
			Metadata: map[string]any{
				"access_token":  "tok-acct1",
				"refresh_token": "rt-acct1",
				"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
			},
		}
		registered, errReg := mgr.Register(WithSkipPersist(ctx), cred)
		if errReg != nil {
			t.Fatalf("Register() error = %v", errReg)
		}

		replaced := registered.Clone()
		replaced.Metadata["access_token"] = "tok-acct2"
		replaced.Metadata["refresh_token"] = "rt-acct2"
		if _, errUpd := mgr.Update(WithSkipPersist(ctx), replaced); errUpd != nil {
			t.Fatalf("Update() error = %v", errUpd)
		}

		mgr.MarkResult(ctx, Result{
			AuthID:            cred.ID,
			Provider:          "antigravity",
			Model:             "gemini-3-flash",
			Success:           false,
			CredentialScope:   true,
			AccessToken:       "tok-acct1",
			RefreshToken:      "rt-acct1",
			CredentialVersion: registered.CredentialVersion,
			RegistrationEpoch: registered.RegistrationEpoch,
			Error:             &Error{HTTPStatus: http.StatusForbidden, Message: validationBody},
		})

		snap, _ := mgr.GetByID(cred.ID)
		if !IsAuthAvailableForModel(snap, "gemini-3-flash", time.Now()) || !IsAuthAvailableForModel(snap, "claude-sonnet-4-6", time.Now()) {
			t.Fatalf("expected old-account 403 VALIDATION_REQUIRED to be discarded after credential replacement: %+v", snap)
		}
	})

	t.Run("credential replacement with empty AccessToken and different RefreshToken discards old 403 VALIDATION_REQUIRED", func(t *testing.T) {
		mgr := NewManager(nil, &RoundRobinSelector{}, nil)
		cred := &Auth{
			ID:       "auth-issue7814-rotate-empty-access-diff-rt",
			Provider: "antigravity",
			Status:   StatusActive,
			Metadata: map[string]any{
				"access_token":  "tok-acct1",
				"refresh_token": "rt-acct1",
				"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
			},
		}
		registered, errReg := mgr.Register(WithSkipPersist(ctx), cred)
		if errReg != nil {
			t.Fatalf("Register() error = %v", errReg)
		}

		replaced := registered.Clone()
		replaced.Metadata["access_token"] = "tok-acct2"
		replaced.Metadata["refresh_token"] = "rt-acct2"
		if _, errUpd := mgr.Update(WithSkipPersist(ctx), replaced); errUpd != nil {
			t.Fatalf("Update() error = %v", errUpd)
		}

		mgr.MarkResult(ctx, Result{
			AuthID:            cred.ID,
			Provider:          "antigravity",
			Model:             "gemini-3-flash",
			Success:           false,
			CredentialScope:   true,
			AccessToken:       "",
			RefreshToken:      "rt-acct1",
			CredentialVersion: registered.CredentialVersion,
			RegistrationEpoch: registered.RegistrationEpoch,
			Error:             &Error{HTTPStatus: http.StatusForbidden, Message: validationBody},
		})

		snap, _ := mgr.GetByID(cred.ID)
		if !IsAuthAvailableForModel(snap, "gemini-3-flash", time.Now()) || !IsAuthAvailableForModel(snap, "claude-sonnet-4-6", time.Now()) {
			t.Fatalf("expected old-account 403 VALIDATION_REQUIRED with empty AccessToken and mismatched RefreshToken to be discarded: %+v", snap)
		}
	})

	t.Run("credential replacement with same AccessToken and different RefreshToken discards old 403 VALIDATION_REQUIRED", func(t *testing.T) {
		mgr := NewManager(nil, &RoundRobinSelector{}, nil)
		cred := &Auth{
			ID:       "auth-issue7814-rotate-same-access-diff-rt",
			Provider: "antigravity",
			Status:   StatusActive,
			Metadata: map[string]any{
				"access_token":  "tok-shared",
				"refresh_token": "rt-acct1",
				"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
			},
		}
		registered, errReg := mgr.Register(WithSkipPersist(ctx), cred)
		if errReg != nil {
			t.Fatalf("Register() error = %v", errReg)
		}

		// Replace refresh_token while temporarily keeping the same access_token (CredentialVersion 1 -> 2).
		replaced := registered.Clone()
		replaced.Metadata["refresh_token"] = "rt-acct2"
		if _, errUpd := mgr.Update(WithSkipPersist(ctx), replaced); errUpd != nil {
			t.Fatalf("Update() error = %v", errUpd)
		}

		mgr.MarkResult(ctx, Result{
			AuthID:            cred.ID,
			Provider:          "antigravity",
			Model:             "gemini-3-flash",
			Success:           false,
			CredentialScope:   true,
			AccessToken:       "tok-shared",
			RefreshToken:      "rt-acct1",
			CredentialVersion: registered.CredentialVersion,
			RegistrationEpoch: registered.RegistrationEpoch,
			Error:             &Error{HTTPStatus: http.StatusForbidden, Message: validationBody},
		})

		snap, _ := mgr.GetByID(cred.ID)
		if !IsAuthAvailableForModel(snap, "gemini-3-flash", time.Now()) || !IsAuthAvailableForModel(snap, "claude-sonnet-4-6", time.Now()) {
			t.Fatalf("expected old-version 403 VALIDATION_REQUIRED with matching AccessToken but mismatched RefreshToken to be discarded: %+v", snap)
		}
	})
}
