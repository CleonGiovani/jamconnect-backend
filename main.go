// =============================================================
// JamConnect Backend — main.go
// Now backed by a real MySQL database running in Docker, instead
// of an in-memory map. This is what fixes the "no account found
// after I already created one" bug: previously, every account
// only ever lived in RAM, so it vanished the instant the server
// process restarted. MySQL persists to disk (inside the Docker
// volume), so accounts survive restarts, redeploys, and your Mac
// rebooting.
//
// Every important event (signup, login, verify, reset) logs a
// line to stdout — visible live in whichever Terminal tab runs
// `go run main.go`, the same way `docker logs <container>` shows
// what a container is doing.
// =============================================================

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// =============================================================
// SECTION: Database
// =============================================================
var db *sql.DB

// Connection details default to matching the `docker run` command
// in the setup instructions exactly, so it works with zero extra
// configuration — but every value can be overridden with an
// environment variable if you ever point this at a different
// MySQL instance (e.g. a hosted one later).
func dbDSN() string {
	host := getEnvOrDefault("DB_HOST", "127.0.0.1")
	port := getEnvOrDefault("DB_PORT", "3306")
	user := getEnvOrDefault("DB_USER", "root")
	pass := getEnvOrDefault("DB_PASSWORD", "devpassword")
	name := getEnvOrDefault("DB_NAME", "jamconnect")
	// parseTime=true lets the driver convert MySQL DATETIME columns
	// directly to/from Go's time.Time — without it, times come back
	// as raw byte strings you'd have to parse yourself.
	return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true", user, pass, host, port, name)
}

func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func initDB() {
	var err error
	db, err = sql.Open("mysql", dbDSN())
	if err != nil {
		log.Fatalf("Failed to open database connection: %v", err)
	}

	// A freshly-started Docker MySQL container can take a few
	// seconds to become ready to accept connections (especially the
	// very first run, while it initializes its data files). Retrying
	// a few times avoids a confusing crash if this server happens to
	// start before MySQL has finished booting.
	var pingErr error
	for attempt := 1; attempt <= 10; attempt++ {
		pingErr = db.Ping()
		if pingErr == nil {
			break
		}
		log.Printf("[DB] Waiting for MySQL to be ready... (attempt %d/10)", attempt)
		time.Sleep(2 * time.Second)
	}
	if pingErr != nil {
		log.Fatalf("Could not connect to MySQL after 10 attempts: %v\n"+
			"Is the Docker container running? Try: docker ps", pingErr)
	}

	schema := []string{
		`CREATE TABLE IF NOT EXISTS users (
			email      VARCHAR(255) PRIMARY KEY,
			password   VARCHAR(255) NOT NULL,
			verified   BOOLEAN NOT NULL DEFAULT FALSE,
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS signup_codes (
			email      VARCHAR(255) PRIMARY KEY,
			code       VARCHAR(16) NOT NULL,
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS reset_codes (
			email      VARCHAR(255) PRIMARY KEY,
			code       VARCHAR(16) NOT NULL,
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS reviews (
			review_id         INT AUTO_INCREMENT PRIMARY KEY,
			provider_email    VARCHAR(255) NOT NULL,
			customer_email    VARCHAR(255) NOT NULL,
			customer_name     VARCHAR(255) NOT NULL,
			rating            INT NOT NULL CHECK (rating BETWEEN 1 AND 5),
			review_text       TEXT NULL,
			provider_response TEXT NULL,
			created_at        DATETIME NOT NULL,
			UNIQUE KEY one_review_per_customer (provider_email, customer_email)
		)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			log.Fatalf("Failed to create tables: %v", err)
		}
	}

	log.Println("[DB] Connected to MySQL, tables ready")
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

var emailRegex = regexp.MustCompile(`^[\w.\-]+@[\w\-]+\.[a-zA-Z]{2,}$`)

// =============================================================
// SECTION: Request/response shapes
// =============================================================

// signUpRequest now carries the full profile, not just credentials.
// FullName/Phone/Parish/UserType are required for everyone. The
// BusinessName through RateType fields only apply when
// UserType == "service_provider" — customers simply leave them blank.
type signUpRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`

	FullName string `json:"fullName"`
	Phone    string `json:"phone"`
	Parish   string `json:"parish"`
	UserType string `json:"userType"` // "customer" or "service_provider"

	// Service-provider-only fields (ignored for customers)
	BusinessName       string  `json:"businessName"`
	Category           string  `json:"category"`
	Description        string  `json:"description"`
	YearsExperience    int     `json:"yearsExperience"`
	TRN                string  `json:"trn"`
	BusinessRegistered bool    `json:"businessRegistered"`
	StartingRate       float64 `json:"startingRate"`
	RateType           string  `json:"rateType"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type verifyRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

type resetPasswordRequest struct {
	Email       string `json:"email"`
	Code        string `json:"code"`
	NewPassword string `json:"newPassword"`
}

type apiResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	// Only populated by a successful login -- lets the frontend show
	// a real "Welcome back, [name]" greeting, and know whether this
	// user is a customer or service provider (e.g. to decide whether
	// to show an "Edit My Profile" option at all).
	FullName string `json:"fullName,omitempty"`
	UserType string `json:"userType,omitempty"`
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
	log.Printf("[SIGNUP] attempt: %s", email)

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

	// --- Profile fields, required for both account types ---
	if strings.TrimSpace(req.FullName) == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Full name is required."})
		return
	}
	if strings.TrimSpace(req.Phone) == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Phone number is required."})
		return
	}
	if strings.TrimSpace(req.Parish) == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Parish is required."})
		return
	}
	if req.UserType != "customer" && req.UserType != "service_provider" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Account type must be 'customer' or 'service_provider'."})
		return
	}
	// --- Extra requirements for service providers only ---
	if req.UserType == "service_provider" {
		if strings.TrimSpace(req.BusinessName) == "" {
			writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Business name is required for service providers."})
			return
		}
		if strings.TrimSpace(req.Category) == "" {
			writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Service category is required for service providers."})
			return
		}
		// Direct tester feedback: claiming "registered" without a TRN
		// is a self-reported claim with no substance behind it -- if
		// you check the box, you must back it up with the number.
		if req.BusinessRegistered && strings.TrimSpace(req.TRN) == "" {
			writeJSON(w, http.StatusBadRequest, apiResponse{Message: "A TRN is required if you're marking your business as registered."})
			return
		}
	}

	var exists int
	err := db.QueryRow(`SELECT 1 FROM users WHERE email = ?`, email).Scan(&exists)
	if err == nil {
		log.Printf("[SIGNUP] rejected, already exists: %s", email)
		writeJSON(w, http.StatusConflict, apiResponse{Message: "An account with this email already exists."})
		return
	} else if err != sql.ErrNoRows {
		log.Printf("[SIGNUP] db error checking existing user: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	if _, err := db.Exec(
		`INSERT INTO users (email, password, verified, created_at, full_name, phone, parish, user_type)
		 VALUES (?, ?, FALSE, ?, ?, ?, ?, ?)`,
		email, req.Password, time.Now(), req.FullName, req.Phone, req.Parish, req.UserType,
	); err != nil {
		log.Printf("[SIGNUP] db error inserting user: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	// Service providers get a second row in service_provider_profiles,
	// holding everything specific to running a business that a
	// customer account simply doesn't need.
	if req.UserType == "service_provider" {
		rateType := req.RateType
		if rateType == "" {
			rateType = "quote" // matches the ENUM default in the schema
		}
		if _, err := db.Exec(
			`INSERT INTO service_provider_profiles
			 (email, business_name, category, description, years_experience, trn, business_registered, starting_rate, rate_type)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			email, req.BusinessName, req.Category, req.Description, req.YearsExperience,
			req.TRN, req.BusinessRegistered, req.StartingRate, rateType,
		); err != nil {
			log.Printf("[SIGNUP] db error inserting service provider profile: %v", err)
			writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
			return
		}
	}

	code := generateCode()
	if _, err := db.Exec(
		`INSERT INTO signup_codes (email, code, created_at) VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE code = VALUES(code), created_at = VALUES(created_at)`,
		email, code, time.Now(),
	); err != nil {
		log.Printf("[SIGNUP] db error storing verification code: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	log.Printf("[SIGNUP] account created, verification pending: %s", email)
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

	var expected string
	err := db.QueryRow(`SELECT code FROM signup_codes WHERE email = ?`, email).Scan(&expected)
	if err != nil || expected != strings.TrimSpace(req.Code) {
		log.Printf("[VERIFY] failed for %s: incorrect or expired code", email)
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "Incorrect or expired verification code."})
		return
	}

	if _, err := db.Exec(`UPDATE users SET verified = TRUE WHERE email = ?`, email); err != nil {
		log.Printf("[VERIFY] db error updating user: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}
	db.Exec(`DELETE FROM signup_codes WHERE email = ?`, email)

	log.Printf("[VERIFY] success: %s", email)
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
	log.Printf("[LOGIN] attempt: %s", email)

	var password string
	var verified bool
	var fullName string
	var userType string
	err := db.QueryRow(`SELECT password, verified, full_name, user_type FROM users WHERE email = ?`, email).
		Scan(&password, &verified, &fullName, &userType)
	if err == sql.ErrNoRows {
		log.Printf("[LOGIN] failed for %s: no account found", email)
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "No account found for this email."})
		return
	} else if err != nil {
		log.Printf("[LOGIN] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	if password != req.Password {
		log.Printf("[LOGIN] failed for %s: incorrect password", email)
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "Incorrect password."})
		return
	}
	if !verified {
		log.Printf("[LOGIN] failed for %s: email not verified", email)
		writeJSON(w, http.StatusForbidden, apiResponse{Message: "Please verify your email before logging in."})
		return
	}

	log.Printf("[LOGIN] success: %s", email)
	writeJSON(w, http.StatusOK, apiResponse{Success: true, Message: "Login successful.", FullName: fullName, UserType: userType})
}

// GET /check-email?email=...
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

	var exists int
	err := db.QueryRow(`SELECT 1 FROM users WHERE email = ?`, email).Scan(&exists)
	writeJSON(w, http.StatusOK, map[string]bool{"exists": err == nil})
}

// providerListing is what each row of GET /providers returns — every
// field the Dashboard's list AND the full profile detail screen need,
// so the frontend never has to make a second request per provider.
type providerListing struct {
	Email              string  `json:"email"`
	FullName           string  `json:"fullName"`
	Phone              string  `json:"phone"`
	Parish             string  `json:"parish"`
	BusinessName       string  `json:"businessName"`
	Category           string  `json:"category"`
	Description        string  `json:"description"`
	YearsExperience    int     `json:"yearsExperience"`
	BusinessRegistered bool    `json:"businessRegistered"`
	StartingRate       float64 `json:"startingRate"`
	RateType           string  `json:"rateType"`
	ProfilePhotoURL    string  `json:"profilePhotoUrl"`
}

// GET /providers
// Returns every VERIFIED service provider — this is the endpoint that
// actually closes the biggest gap in the app: previously, the
// Dashboard showed 8 hardcoded fake businesses that never changed no
// matter who signed up. Now it reflects real accounts. Unverified
// providers are deliberately excluded — showing an account before its
// owner confirmed their email would let anyone list a fake business.
func providersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "Use GET"})
		return
	}

	rows, err := db.Query(`
		SELECT u.email, u.full_name, u.phone, u.parish, u.profile_photo_url,
		       p.business_name, p.category, p.description,
		       p.years_experience, p.business_registered,
		       p.starting_rate, p.rate_type
		FROM users u
		JOIN service_provider_profiles p ON p.email = u.email
		WHERE u.verified = TRUE
		ORDER BY p.business_name
	`)
	if err != nil {
		log.Printf("[PROVIDERS] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}
	defer rows.Close()

	providers := []providerListing{} // starts as [] not null, so empty JSON is "[]" not "null"
	for rows.Next() {
		var p providerListing
		var photoURL sql.NullString
		if err := rows.Scan(
			&p.Email, &p.FullName, &p.Phone, &p.Parish, &photoURL,
			&p.BusinessName, &p.Category, &p.Description,
			&p.YearsExperience, &p.BusinessRegistered,
			&p.StartingRate, &p.RateType,
		); err != nil {
			log.Printf("[PROVIDERS] row scan error: %v", err)
			continue
		}
		if photoURL.Valid {
			p.ProfilePhotoURL = photoURL.String
		}
		providers = append(providers, p)
	}

	// rows.Next() returning false means either "no more rows" (normal)
	// or "something went wrong mid-read" (e.g. connection dropped) --
	// checking rows.Err() afterward is how you tell those apart. Without
	// this, a dropped connection partway through would silently return
	// a truncated list with no error at all.
	if err := rows.Err(); err != nil {
		log.Printf("[PROVIDERS] error while reading rows: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	writeJSON(w, http.StatusOK, providers)
}

// updateProfileRequest carries both the fields being changed AND the
// current password -- since this app has no session-token system,
// requiring the password again is the honest way to confirm "you
// actually own this account" before allowing a write to it. Without
// this check, anyone who knew a provider's email could edit their
// listing, since nothing else identifies who's making the request.
type updateProfileRequest struct {
	Email              string  `json:"email"`
	Password           string  `json:"password"`
	BusinessName       string  `json:"businessName"`
	Category           string  `json:"category"`
	Description        string  `json:"description"`
	YearsExperience    int     `json:"yearsExperience"`
	TRN                string  `json:"trn"`
	BusinessRegistered bool    `json:"businessRegistered"`
	StartingRate       float64 `json:"startingRate"`
	RateType           string  `json:"rateType"`
}

// POST /providers/update
func updateProfileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "Use POST"})
		return
	}

	var req updateProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Invalid request body"})
		return
	}

	email := normalizeEmail(req.Email)

	// Same password check as login -- this IS the authorization check
	// for this endpoint, not just a login formality.
	var storedPassword string
	err := db.QueryRow(`SELECT password FROM users WHERE email = ?`, email).Scan(&storedPassword)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "No account found for this email."})
		return
	} else if err != nil {
		log.Printf("[UPDATE-PROFILE] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}
	if storedPassword != req.Password {
		log.Printf("[UPDATE-PROFILE] failed for %s: incorrect password", email)
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "Incorrect password."})
		return
	}

	// Same validation rules as signup -- editing shouldn't be able to
	// produce an account state that signing up wouldn't have allowed.
	if strings.TrimSpace(req.BusinessName) == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Business name is required."})
		return
	}
	if strings.TrimSpace(req.Category) == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Service category is required."})
		return
	}
	if req.BusinessRegistered && strings.TrimSpace(req.TRN) == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "A TRN is required if you're marking your business as registered."})
		return
	}

	rateType := req.RateType
	if rateType == "" {
		rateType = "quote"
	}

	if _, err := db.Exec(
		`UPDATE service_provider_profiles
		 SET business_name = ?, category = ?, description = ?, years_experience = ?,
		     trn = ?, business_registered = ?, starting_rate = ?, rate_type = ?
		 WHERE email = ?`,
		req.BusinessName, req.Category, req.Description, req.YearsExperience,
		req.TRN, req.BusinessRegistered, req.StartingRate, rateType, email,
	); err != nil {
		log.Printf("[UPDATE-PROFILE] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	log.Printf("[UPDATE-PROFILE] success: %s", email)
	writeJSON(w, http.StatusOK, apiResponse{Success: true, Message: "Profile updated successfully."})
}

// =============================================================
// SECTION: Reviews
// =============================================================

type reviewListing struct {
	ReviewID         int    `json:"reviewId"`
	CustomerName     string `json:"customerName"`
	Rating           int    `json:"rating"`
	ReviewText       string `json:"reviewText"`
	ProviderResponse string `json:"providerResponse"`
	CreatedAt        string `json:"createdAt"`
	// Included so the frontend can tell "is this my own review" (to
	// hide the leave-a-review button after already reviewing) without
	// a second request.
	CustomerEmail    string `json:"customerEmail"`
	CustomerPhotoURL string `json:"customerPhotoUrl"`
}

type submitReviewRequest struct {
	ProviderEmail string `json:"providerEmail"`
	CustomerEmail string `json:"customerEmail"`
	Rating        int    `json:"rating"`
	ReviewText    string `json:"reviewText"`
}

// POST /reviews/submit
// Deliberately NOT password-gated, unlike /providers/update and
// /reviews/respond -- leaving a review is low-stakes enough that
// re-confirming a password once already logged in was pure friction
// with little real benefit. Still checks the account genuinely
// exists (so a review can't be attributed to a made-up email), just
// not that this specific request proves ongoing ownership of it.
func submitReviewHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "Use POST"})
		return
	}

	var req submitReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Invalid request body"})
		return
	}

	providerEmail := normalizeEmail(req.ProviderEmail)
	customerEmail := normalizeEmail(req.CustomerEmail)

	if providerEmail == customerEmail {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "You can't review your own listing."})
		return
	}
	if req.Rating < 1 || req.Rating > 5 {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Rating must be between 1 and 5."})
		return
	}

	var customerName string
	err := db.QueryRow(`SELECT full_name FROM users WHERE email = ?`, customerEmail).Scan(&customerName)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "No account found for this email."})
		return
	} else if err != nil {
		log.Printf("[SUBMIT-REVIEW] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	_, err = db.Exec(
		`INSERT INTO reviews (provider_email, customer_email, customer_name, rating, review_text, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		providerEmail, customerEmail, customerName, req.Rating, req.ReviewText, time.Now(),
	)
	if err != nil {
		// The UNIQUE KEY on (provider_email, customer_email) is what
		// makes a duplicate insert fail here -- this is the "one
		// review per customer per provider" rule being enforced by
		// the database itself, not just application logic.
		if strings.Contains(err.Error(), "Duplicate entry") {
			writeJSON(w, http.StatusConflict, apiResponse{Message: "You've already reviewed this provider."})
			return
		}
		log.Printf("[SUBMIT-REVIEW] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	log.Printf("[SUBMIT-REVIEW] success: %s reviewed %s", customerEmail, providerEmail)
	writeJSON(w, http.StatusCreated, apiResponse{Success: true, Message: "Review submitted."})
}

// GET /reviews?provider=EMAIL
func fetchReviewsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "Use GET"})
		return
	}

	providerEmail := normalizeEmail(r.URL.Query().Get("provider"))
	if providerEmail == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Missing provider query parameter."})
		return
	}

	rows, err := db.Query(
		`SELECT r.review_id, r.customer_name, r.rating, r.review_text, r.provider_response,
		        r.created_at, r.customer_email, u.profile_photo_url
		 FROM reviews r
		 JOIN users u ON u.email = r.customer_email
		 WHERE r.provider_email = ? ORDER BY r.created_at DESC`,
		providerEmail,
	)
	if err != nil {
		log.Printf("[FETCH-REVIEWS] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}
	defer rows.Close()

	reviews := []reviewListing{}
	for rows.Next() {
		var rv reviewListing
		var response, photoURL sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&rv.ReviewID, &rv.CustomerName, &rv.Rating, &rv.ReviewText,
			&response, &createdAt, &rv.CustomerEmail, &photoURL); err != nil {
			log.Printf("[FETCH-REVIEWS] row scan error: %v", err)
			continue
		}
		if photoURL.Valid {
			rv.CustomerPhotoURL = photoURL.String
		}
		if response.Valid {
			rv.ProviderResponse = response.String
		}
		rv.CreatedAt = createdAt.Format("2006-01-02")
		reviews = append(reviews, rv)
	}
	if err := rows.Err(); err != nil {
		log.Printf("[FETCH-REVIEWS] error while reading rows: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	writeJSON(w, http.StatusOK, reviews)
}

type respondReviewRequest struct {
	ReviewID         int    `json:"reviewId"`
	ProviderEmail    string `json:"providerEmail"`
	ProviderPassword string `json:"providerPassword"`
	ResponseText     string `json:"responseText"`
}

// POST /reviews/respond
// Lets a provider respond to a review left on their own listing --
// deliberately does NOT allow deleting or editing the review itself,
// only adding a reply, matching the tester's specific feedback that
// providers should be able to respond, not remove unwanted reviews.
func respondReviewHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "Use POST"})
		return
	}

	var req respondReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Invalid request body"})
		return
	}

	providerEmail := normalizeEmail(req.ProviderEmail)

	var storedPassword string
	err := db.QueryRow(`SELECT password FROM users WHERE email = ?`, providerEmail).Scan(&storedPassword)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "No account found for this email."})
		return
	} else if err != nil {
		log.Printf("[RESPOND-REVIEW] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}
	if storedPassword != req.ProviderPassword {
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "Incorrect password."})
		return
	}

	// The WHERE clause checks provider_email too, not just review_id --
	// this is what stops a provider from responding to a review left
	// on a DIFFERENT provider's listing, not just password-checking.
	result, err := db.Exec(
		`UPDATE reviews SET provider_response = ? WHERE review_id = ? AND provider_email = ?`,
		req.ResponseText, req.ReviewID, providerEmail,
	)
	if err != nil {
		log.Printf("[RESPOND-REVIEW] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		writeJSON(w, http.StatusNotFound, apiResponse{Message: "Review not found, or it isn't on your own listing."})
		return
	}

	log.Printf("[RESPOND-REVIEW] success: %s responded to review %d", providerEmail, req.ReviewID)
	writeJSON(w, http.StatusOK, apiResponse{Success: true, Message: "Response saved."})
}

// =============================================================
// SECTION: Profile photos
// Photos are stored on local disk (same as everything else in this
// app -- MySQL runs in a local Docker container, not a hosted
// service), under uploads/profile-photos/. Filenames are derived
// from the account's email so a re-upload cleanly replaces the old
// photo rather than accumulating files.
// =============================================================

const uploadsDir = "uploads/profile-photos"

var allowedPhotoExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
}

// sanitizeEmailForFilename turns an email into something safe to use
// as a filename -- @ and . aren't valid/wise in filenames on every
// OS, so they get replaced with underscores.
func sanitizeEmailForFilename(email string) string {
	replacer := strings.NewReplacer("@", "_at_", ".", "_")
	return replacer.Replace(email)
}

// POST /profile-photo  (multipart/form-data: email, photo)
// Deliberately NOT password-gated, same reasoning as
// /reviews/submit -- once already logged in, re-confirming a
// password to upload your own photo is friction with little real
// benefit. Still checks the account genuinely exists.
func uploadProfilePhotoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "Use POST"})
		return
	}

	// 10MB max -- generous for a profile photo, small enough to not
	// let someone accidentally (or deliberately) fill up local disk.
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "File too large (max 10MB) or invalid form."})
		return
	}

	email := normalizeEmail(r.FormValue("email"))

	var exists int
	err := db.QueryRow(`SELECT 1 FROM users WHERE email = ?`, email).Scan(&exists)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "No account found for this email."})
		return
	} else if err != nil {
		log.Printf("[UPLOAD-PHOTO] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	file, header, err := r.FormFile("photo")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "No photo file provided."})
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	if !allowedPhotoExtensions[ext] {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Only JPG, PNG, GIF, or WEBP images are allowed."})
		return
	}

	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		log.Printf("[UPLOAD-PHOTO] could not create uploads dir: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	filename := sanitizeEmailForFilename(email) + ext
	destPath := filepath.Join(uploadsDir, filename)

	dest, err := os.Create(destPath)
	if err != nil {
		log.Printf("[UPLOAD-PHOTO] could not create file: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}
	defer dest.Close()

	if _, err := io.Copy(dest, file); err != nil {
		log.Printf("[UPLOAD-PHOTO] could not save file: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	photoURL := "/uploads/profile-photos/" + filename
	if _, err := db.Exec(`UPDATE users SET profile_photo_url = ? WHERE email = ?`, photoURL, email); err != nil {
		log.Printf("[UPLOAD-PHOTO] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	log.Printf("[UPLOAD-PHOTO] success: %s", email)
	writeJSON(w, http.StatusOK, map[string]string{"photoUrl": photoURL})
}

type myProfileRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type myProfileResponse struct {
	Success         bool   `json:"success"`
	Message         string `json:"message,omitempty"`
	FullName        string `json:"fullName"`
	Email           string `json:"email"`
	Phone           string `json:"phone"`
	Parish          string `json:"parish"`
	UserType        string `json:"userType"`
	ProfilePhotoURL string `json:"profilePhotoUrl"`
}

// POST /me  { "email": "...", "password": "..." }
// Lets a logged-in user fetch their own current details -- used by
// both the customer and provider "my profile" screens to show the
// current photo (if any) before letting them replace it.
func myProfileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "Use POST"})
		return
	}

	var req myProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Invalid request body"})
		return
	}

	email := normalizeEmail(req.Email)

	var storedPassword, fullName, phone, parish, userType string
	var photoURL sql.NullString
	err := db.QueryRow(
		`SELECT password, full_name, phone, parish, user_type, profile_photo_url FROM users WHERE email = ?`,
		email,
	).Scan(&storedPassword, &fullName, &phone, &parish, &userType, &photoURL)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "No account found for this email."})
		return
	} else if err != nil {
		log.Printf("[MY-PROFILE] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}
	if storedPassword != req.Password {
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "Incorrect password."})
		return
	}

	resp := myProfileResponse{
		Success:  true,
		FullName: fullName,
		Email:    email,
		Phone:    phone,
		Parish:   parish,
		UserType: userType,
	}
	if photoURL.Valid {
		resp.ProfilePhotoURL = photoURL.String
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /resend-code  { "email": "..." }
func resendCodeHandler(w http.ResponseWriter, r *http.Request) {
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

	var verified bool
	err := db.QueryRow(`SELECT verified FROM users WHERE email = ?`, email).Scan(&verified)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, apiResponse{Message: "No account found for this email."})
		return
	} else if err != nil {
		log.Printf("[RESEND] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}
	if verified {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "This account is already verified."})
		return
	}

	code := generateCode()
	if _, err := db.Exec(
		`INSERT INTO signup_codes (email, code, created_at) VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE code = VALUES(code), created_at = VALUES(created_at)`,
		email, code, time.Now(),
	); err != nil {
		log.Printf("[RESEND] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	log.Printf("[RESEND] new code generated: %s", email)
	writeJSON(w, http.StatusOK, apiResponse{
		Success: true,
		Message: "New verification code generated (simulated email).",
		Code:    code,
	})
}

func generateCode() string {
	return fmt.Sprintf("%06d", rand.Intn(1000000))
}

// POST /forgot-password  { "email": "..." }
func forgotPasswordHandler(w http.ResponseWriter, r *http.Request) {
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
	code := generateCode()

	if _, err := db.Exec(
		`INSERT INTO reset_codes (email, code, created_at) VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE code = VALUES(code), created_at = VALUES(created_at)`,
		email, code, time.Now(),
	); err != nil {
		log.Printf("[FORGOT-PASSWORD] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}

	log.Printf("[FORGOT-PASSWORD] reset code generated for: %s", email)
	writeJSON(w, http.StatusOK, apiResponse{
		Success: true,
		Message: "If that account exists, a reset code has been generated (simulated email).",
		Code:    code,
	})
}

// POST /reset-password  { "email": "...", "code": "...", "newPassword": "..." }
func resetPasswordHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "Use POST"})
		return
	}

	var req resetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Invalid request body"})
		return
	}

	email := normalizeEmail(req.Email)

	var expected string
	err := db.QueryRow(`SELECT code FROM reset_codes WHERE email = ?`, email).Scan(&expected)
	if err != nil || expected != strings.TrimSpace(req.Code) {
		log.Printf("[RESET-PASSWORD] failed for %s: incorrect or expired code", email)
		writeJSON(w, http.StatusUnauthorized, apiResponse{Message: "Incorrect or expired reset code."})
		return
	}

	if len(req.NewPassword) < 15 {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Password must be at least 15 characters long."})
		return
	}
	if len(req.NewPassword) > 64 {
		writeJSON(w, http.StatusBadRequest, apiResponse{Message: "Password must be 64 characters or fewer."})
		return
	}

	if _, err := db.Exec(`UPDATE users SET password = ? WHERE email = ?`, req.NewPassword, email); err != nil {
		log.Printf("[RESET-PASSWORD] db error: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiResponse{Message: "Server error, please try again."})
		return
	}
	db.Exec(`DELETE FROM reset_codes WHERE email = ?`, email)

	log.Printf("[RESET-PASSWORD] success: %s", email)
	writeJSON(w, http.StatusOK, apiResponse{Success: true, Message: "Password reset successfully."})
}

// =============================================================
// SECTION: CORS middleware
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
func main() {
	initDB()
	defer db.Close()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/health", withCORS(healthHandler))
	http.HandleFunc("/signup", withCORS(signUpHandler))
	http.HandleFunc("/verify", withCORS(verifyHandler))
	http.HandleFunc("/login", withCORS(loginHandler))
	http.HandleFunc("/check-email", withCORS(checkEmailHandler))
	http.HandleFunc("/providers", withCORS(providersHandler))
	http.HandleFunc("/providers/update", withCORS(updateProfileHandler))
	http.HandleFunc("/reviews", withCORS(fetchReviewsHandler))
	http.HandleFunc("/reviews/submit", withCORS(submitReviewHandler))
	http.HandleFunc("/reviews/respond", withCORS(respondReviewHandler))
	http.HandleFunc("/profile-photo", withCORS(uploadProfilePhotoHandler))
	http.HandleFunc("/me", withCORS(myProfileHandler))

	// Serves uploaded photos back out as plain static files, e.g. a
	// file saved as uploads/profile-photos/foo.jpg becomes reachable
	// at /uploads/profile-photos/foo.jpg. Wrapped in the same CORS
	// middleware as everything else -- without it, the Flutter web
	// app (a different origin) wouldn't be allowed to load the images.
	fileServer := http.FileServer(http.Dir("uploads"))
	http.Handle("/uploads/", withCORS(http.StripPrefix("/uploads/", fileServer).ServeHTTP))
	http.HandleFunc("/resend-code", withCORS(resendCodeHandler))
	http.HandleFunc("/forgot-password", withCORS(forgotPasswordHandler))
	http.HandleFunc("/reset-password", withCORS(resetPasswordHandler))

	log.Printf("JamConnect backend starting on port %s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
