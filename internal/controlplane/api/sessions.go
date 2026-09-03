package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
	"github.com/danmorcov88/fleetward/internal/telemetry"
)

// Session establishment is two hand-written HTTP handlers rather than RPCs on IdentityService, and
// the reason is the same one that has always kept `/readyz` out of the contract (ADR-0029): a
// cookie is a transport concern that Protobuf cannot express. A gRPC method would have to smuggle
// `Set-Cookie` out through response metadata and a header matcher, which is more machinery than the
// thing it would be describing.
//
// What these two handlers are *for* is the seam B10 walks through. Today `POST /api/v1/sessions`
// exchanges an API token for a cookie. When OIDC lands it exchanges an authorization code for the
// same cookie, at the same path, and nothing downstream — the guard, the grants, the audit log, the
// UI's own code — changes at all (ADR-0033).

// RegisterSessions mounts the session endpoints.
//
// They are deliberately outside the guarded gateway: a caller establishing a session has no
// credential yet by definition, so a route that required one could never be reached.
func (s *Server) RegisterSessions(sessions *authn.Sessions, tokens *authn.TokenStore) {
	s.mux.HandleFunc("POST /api/v1/sessions", s.handleCreateSession(sessions, tokens))
	s.mux.HandleFunc("DELETE /api/v1/sessions", s.handleDeleteSession(sessions))
}

type createSessionRequest struct {
	Token string `json:"token"`
}

type createSessionResponse struct {
	Actor       string `json:"actor"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	ExpiresIn   int    `json:"expires_in"`
}

func (s *Server) handleCreateSession(sessions *authn.Sessions, tokens *authn.TokenStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		requestID := telemetry.RequestIDFrom(ctx)

		// Bounded because this endpoint is reachable without a credential, so its body is the one
		// piece of untrusted input the control plane reads before authenticating anything.
		var req createSessionRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
			WriteProblem(w, http.StatusBadRequest, "Bad Request",
				"the request body must be JSON of the form {\"token\": \"fwt_…\"}", requestID)
			return
		}
		if req.Token == "" {
			WriteProblem(w, http.StatusBadRequest, "Bad Request",
				"a token is required; create one with `fleetward-cli token create`", requestID)
			return
		}

		principal, err := tokens.Verify(ctx, req.Token)
		if err != nil {
			// One message for every way of being wrong. Distinguishing "unknown" from "revoked"
			// would tell whoever is guessing that they had guessed a real token.
			s.log.WarnContext(ctx, "rejected a token at session creation",
				slog.String("remote_addr", clientIP(r)))
			WriteProblem(w, http.StatusUnauthorized, "Unauthorized",
				"that token is not valid", requestID)
			return
		}

		cookie, err := sessions.Issue(principal)
		if err != nil {
			s.log.ErrorContext(ctx, "could not issue a session", slog.String("error", err.Error()))
			WriteProblem(w, http.StatusInternalServerError, "Internal Server Error",
				"the session could not be issued", requestID)
			return
		}
		http.SetCookie(w, cookie)

		// The response deliberately does not echo the token back. The browser now holds an HttpOnly
		// cookie and has no reason to keep the credential it was given, and a UI that could read
		// one from a response body is a UI that could store it somewhere a script can reach.
		writeJSON(w, http.StatusOK, createSessionResponse{
			Actor:       principal.Actor,
			DisplayName: principal.DisplayName,
			Email:       principal.Email,
			ExpiresIn:   cookie.MaxAge,
		})
	}
}

func (s *Server) handleDeleteSession(sessions *authn.Sessions) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		// Unauthenticated on purpose: signing out is not an action that needs permission, and a
		// sign-out that failed because the session had already expired would leave the cookie in
		// place, which is the opposite of what the button is for.
		http.SetCookie(w, sessions.Clear())
		w.WriteHeader(http.StatusNoContent)
	}
}
