// =============================================================
// JamConnect Backend — main.go
// This is the entry point for your Go server. Right now it just
// has one endpoint (/health) to prove the server runs and Flutter
// can reach it. We'll add /signup, /login, and /verify next,
// each mirroring the logic currently living in AuthService on
// the Flutter side — except this time, data will live on the
// server instead of the device.
// =============================================================

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
)

// =============================================================
// SECTION: In-memory data store
// Real backends use a database (MySQL, Postgres, etc.) so data
// survives a server restart. For now, we're keeping everything
// in memory (a Go map) so you can focus on the request/response
// logic first — we'll swap this for a real database once this
// is working end-to-end. Because Go can handle multiple requests
// at once, we use a Mutex ("mutual exclusion" lock) to prevent
// two requests from corrupting the map at the same time.
// =============================================================

type user struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Verified bool   `json:"verified"`
}

var (
	usersMu   sync.Mutex
	users     = map[string]*user{} // key: normalized email
	pendingMu sync.Mutex
	pending   = map[string]string{} // key: email, value: verification code
)

var emailRegex = regexp.MustCompile(`^[\w.\-]+@[\w\-]+\.[a-zA-Z]{2,}$`)

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// =============================================================
// SECTION: Request/response shapes
// These structs define exactly what JSON a request must contain
// and what JSON a response will return — Go's equivalent of the
// jsonDecode/jsonEncode work AuthService does in Dart, but with
// compile-time type checking instead of a loose Map.
// =============================================================

type signUpRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type verifyRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

type apiResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	// Code is only populated in the sign-up response, simulating
	// what would normally be emailed to the user.
	Code string `json:"code,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

// =============================================================
// SECTION: Handlers
// =============================================================

func healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": "JamConnect backend is running",
	})
}

// POST /signup  { "email": "...", "password": "..." }
// Validates email format + password strength, rejects duplicates,
// creates an unverified account, and generates a verification
// code (returned directly in the response for now — simulating
// an email send, same approach as the Flutter-only version).
func signUpHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "Use POST"})
		return
	}

	var req signUpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Invalid request body"})
		return
	}

	email := normalizeEmail(req.Email)

	if email == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Email and password cannot be empty."})
		return
	}
	if !emailRegex.MatchString(email) {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Please enter a valid email address."})
		return
	}
	if len(req.Password) < 15 {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Password must be at least 15 characters long."})
		return
	}
	if len(req.Password) > 64 {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Password must be 64 characters or fewer."})
		return
	}

	usersMu.Lock()
	_, exists := users[email]
	if exists {
		usersMu.Unlock()
		writeJSON(w, http.StatusConflict, apiResponse{Message: "An account with this email already exists."})
		return
	}
	users[email] = &user{Email: email, Password: req.Password, Verified: false}
	usersMu.Unlock()

	code := generateCode()
	pendingMu.Lock()
	pending[email] = code
	pendingMu.Unlock()

	writeJSON(w, http.StatusCreated, apiResponse{
		Success: true,
		Message: "Account created. Verification code generated (simulated email).",
		Code:    code,
	})
}

// POST /verify  { "email": "...", "code": "..." }
func verifyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "Use POST"})
		return
	}

	var req verifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Invalid request body"})
		return
	}

	email := normalizeEmail(req.Email)

	pendingMu.Lock()
	expected, ok := pending[email]
	pendingMu.Unlock()

	if !ok || expected != strings.TrimSpace(req.Code) {
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "Incorrect or expired verification code."})
		return
	}

	usersMu.Lock()
	if u, exists := users[email]; exists {
		u.Verified = true
	}
	usersMu.Unlock()

	pendingMu.Lock()
	delete(pending, email)
	pendingMu.Unlock()

	writeJSON(w, http.StatusOK, apiResponse{Success: true, Message: "Email verified successfully."})
}

// POST /login  { "email": "...", "password": "..." }
func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "Use POST"})
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Invalid request body"})
		return
	}

	email := normalizeEmail(req.Email)

	usersMu.Lock()
	u, exists := users[email]
	usersMu.Unlock()

	if !exists {
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "No account found for this email."})
		return
	}
	if u.Password != req.Password {
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "Incorrect password."})
		return
	}
	if !u.Verified {
		writeJSON(w, http.StatusForbidden, apiResponse{Message: "Please verify your email before logging in."})
		return
	}

	writeJSON(w, http.StatusOK, apiResponse{Success: true, Message: "Login successful."})
}

// GET /check-email?email=...
// Looks up whether an account exists for this email, WITHOUT
// requiring a password. Used by the sign-up screen to warn the
// user early that an email is already taken, before they type
// out a whole password.
//
// Security note: publicly confirming "this email exists" is
// known as account enumeration — it lets someone probe which
// emails are registered. That's an accepted tradeoff for a
// sign-up "is this taken?" check (most real apps do this), but
// you would NOT do the same thing on a password-reset flow —
// there, best practice is to always say "if this email exists,
// we've sent a link" regardless of the real answer.
func checkEmailHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "Use GET"})
		return
	}

	email := normalizeEmail(r.URL.Query().Get("email"))
	if email == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Missing email query parameter."})
		return
	}

	usersMu.Lock()
	_, exists := users[email]
	usersMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]bool{"exists": exists})
}

// POST /resend-code  { "email": "..." }
// Generates a fresh verification code for an existing, unverified
// account and returns it (simulated email). Useful if the original
// code was missed — which is easy to do, since it's only ever shown
// briefly in the app rather than actually emailed.
func resendCodeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "Use POST"})
		return
	}

	var req signUpRequest // only the Email field is used here
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Invalid request body"})
		return
	}

	email := normalizeEmail(req.Email)

	usersMu.Lock()
	u, exists := users[email]
	usersMu.Unlock()

	if !exists {
		writeJSON(w, http.StatusNotFound, apiResponse{Message: "No account found for this email."})
		return
	}
	if u.Verified {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "This account is already verified."})
		return
	}

	code := generateCode()
	pendingMu.Lock()
	pending[email] = code
	pendingMu.Unlock()

	writeJSON(w, http.StatusOK, apiResponse{
		Success: true,
		Message: "New verification code generated (simulated email).",
		Code:    code,
	})
}

func generateCode() string {
	return fmt.Sprintf("%06d", rand.Intn(1000000))
}

// =============================================================
// SECTION: CORS middleware
// Your Flutter web app runs on a different port (e.g. localhost:
// 51882) than this server (localhost:8080). Browsers block
// cross-origin requests by default unless the server explicitly
// allows it — this wrapper adds the headers that permit it.
// Without this, Flutter's http requests to this server would
// fail silently or throw a CORS error in the browser console.
// =============================================================
func withCORS(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		handler(w, r)
	}
}

// --- Entry point ---
// Hosting platforms like Render assign a port dynamically and tell
// your app which one to use via the PORT environment variable — your
// server MUST listen on that port, not a hardcoded one, or the
// platform won't be able to route traffic to it. Locally, no PORT
// variable is set, so we fall back to 8080 like before.
func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/health", withCORS(healthHandler))
	http.HandleFunc("/signup", withCORS(signUpHandler))
	http.HandleFunc("/verify", withCORS(verifyHandler))
	http.HandleFunc("/login", withCORS(loginHandler))
	http.HandleFunc("/check-email", withCORS(checkEmailHandler))
	http.HandleFunc("/resend-code", withCORS(resendCodeHandler))

	log.Printf("JamConnect backend starting on port %s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
