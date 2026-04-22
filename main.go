package main

import (
	_ "embed"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	_ "github.com/jackc/pgx/v5/stdlib"
	razorpay "github.com/razorpay/razorpay-go"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

//go:embed index.html
var indexHTML []byte

const systemPrompt = `You are a meme generator. Given a situation, pick the best template and write captions that are short, punchy, and genuinely funny.

Respond ONLY with valid JSON, no markdown, no explanation:
{"template_id": "<id>", "top_text": "<caption>", "bottom_text": "<caption>"}

Available templates — pick the one that best fits the emotion or dynamic of the situation:

181913649 - Drake approving/rejecting: use for preference comparison, someone choosing the worse/funnier option over the sensible one
87743020 - Two Buttons: use when someone is sweating between two equally bad or contradictory choices
112126428 - Distracted Boyfriend: use for temptation or betrayal, person ignoring one thing for another
124822590 - Left Exit 12 Off Ramp: use for sudden swerve to avoid something, impulsive decisions
217743513 - UNO Draw 25: use for extreme avoidance, I'd rather do anything than the obvious thing
131087935 - Running Away Balloon: use for ignoring real problems, labelling things you pretend don't exist
247375501 - Buff Doge vs Cheems: use for confident version vs weak version of the same thing
129242436 - Change My Mind: use for stating a controversial or unpopular opinion confidently
188390779 - Woman Yelling at Cat: use for overreaction, someone dramatic vs something completely unbothered
438680 - Batman Slapping Robin: use for correcting someone mid-sentence, shutting down a bad take
100777631 - Is This A Pigeon: use for confidently misidentifying something, wrong assumption played straight
93895088 - Expanding Brain: use for escalating ideas from sensible to galaxy brain
80707627 - Sad Pablo Escobar: use for alone and waiting, boredom or abandonment
4087833 - Waiting Skeleton: use for waiting so long you've turned to bones, extreme delays
222403160 - Bernie I Am Once Again Asking: use for repeatedly asking for the same thing and never getting it
178591752 - Tuxedo Winnie the Pooh: use for basic thing vs unnecessarily fancy version of the same thing
135256802 - Epic Handshake: use for two unlikely things agreeing on something unexpected
101470 - Ancient Aliens Guy: use for wildly blaming something inexplicable on a single absurd cause
61579 - One Does Not Simply: use for something that sounds simple but is actually impossible
14371066 - Yoda: use for wise reversal, stating something in the wrong order for comic effect
79132341 - Bike Fall: use for self-sabotage, person causing their own problem while someone watches
252600902 - Always Has Been: use for revealing something was always true, the astronaut meme
148909805 - Monkey Puppet: use for side-eye awkward look, slowly turning away from something uncomfortable
322841258 - Anakin Padme 4 Panel: use for doing something expecting one outcome but getting an unexpected result
28251713 - Oprah You Get A: use for giving the same thing to everyone indiscriminately, chaos mode
55311130 - This Is Fine: use for calmly ignoring catastrophic problems around you
102156234 - Mocking Spongebob: use for mockingly repeating what someone said in a dumb voice
226297822 - Panik Kalm Panik: use for initial panic, false relief, then worse panic
89370399 - Roll Safe Think About It: use for technically correct but deeply flawed logic

Rules for captions:
- Keep top and bottom text short — under 8 words each
- Be specific to the situation, never generic
- Punch line goes on the bottom
- If the template only needs one caption put the joke on bottom and leave top empty
- Never explain the joke`

type memeRequest struct {
	Prompt string `json:"prompt"`
}

type feedbackRequest struct {
	Message string `json:"message"`
}

type memeResponse struct {
	MemeURL     string `json:"meme_url,omitempty"`
	Error       string `json:"error,omitempty"`
	CreditsLeft *int   `json:"credits_left,omitempty"`
}

type user struct {
	ID      int
	Email   string
	Name    string
	Credits int
}

type pack struct {
	Credits int
	Amount  int // paise (1 INR = 100 paise)
	Label   string
}

var packs = map[string]pack{
	"starter": {Credits: 10, Amount: 900, Label: "₹9 — 10 memes"},
	"popular": {Credits: 25, Amount: 1900, Label: "₹19 — 25 memes"},
	"stash":   {Credits: 75, Amount: 4900, Label: "₹49 — 75 memes"},
}

type orderRequest struct {
	Pack string `json:"pack"`
}

type verifyRequest struct {
	OrderID   string `json:"order_id"`
	PaymentID string `json:"payment_id"`
	Signature string `json:"signature"`
	Pack      string `json:"pack"`
}

type claudeChoice struct {
	TemplateID string `json:"template_id"`
	TopText    string `json:"top_text"`
	BottomText string `json:"bottom_text"`
}

type imgflipResponse struct {
	Success bool `json:"success"`
	Data    struct {
		URL string `json:"url"`
	} `json:"data"`
	ErrorMessage string `json:"error_message"`
}

type rateLimiter struct {
	mu      sync.Mutex
	clients map[string][]time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{clients: make(map[string][]time.Time)}
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-2 * time.Minute)

	var recent []time.Time
	for _, t := range rl.clients[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}

	if len(recent) >= 10 {
		rl.clients[ip] = recent
		return false
	}

	rl.clients[ip] = append(recent, now)
	return true
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.TrimSpace(strings.SplitN(fwd, ",", 2)[0])
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

func main() {
	for _, v := range []string{
		"ANTHROPIC_API_KEY", "IMGFLIP_USERNAME", "IMGFLIP_PASSWORD",
		"DATABASE_URL", "SESSION_SECRET",
		"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "BASE_URL",
		"RAZORPAY_KEY_ID", "RAZORPAY_KEY_SECRET",
	} {
		if os.Getenv(v) == "" {
			log.Fatalf("Required environment variable %s is not set", v)
		}
	}

	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS feedback (
			id         SERIAL PRIMARY KEY,
			message    TEXT NOT NULL,
			ip         TEXT,
			created_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS users (
			id         SERIAL PRIMARY KEY,
			google_id  TEXT UNIQUE NOT NULL,
			email      TEXT NOT NULL,
			name       TEXT,
			credits    INT NOT NULL DEFAULT 7,
			created_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS sessions (
			id         TEXT PRIMARY KEY,
			user_id    INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at TIMESTAMPTZ NOT NULL
		);
	`); err != nil {
		log.Fatalf("Failed to create tables: %v", err)
	}

	oauthCfg := &oauth2.Config{
		ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		RedirectURL:  os.Getenv("BASE_URL") + "/auth/google/callback",
		Scopes:       []string{"openid", "email", "profile"},
		Endpoint:     google.Endpoint,
	}

	client := anthropic.NewClient()
	rl := newRateLimiter()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", serveIndex)
	mux.HandleFunc("POST /api/meme", handleMeme(client, rl, db))
	mux.HandleFunc("GET /api/download", handleDownload)
	mux.HandleFunc("POST /api/feedback", handleFeedback(db))
	mux.HandleFunc("GET /api/me", handleMe(db))
	mux.HandleFunc("GET /auth/google", handleGoogleLogin(oauthCfg))
	mux.HandleFunc("GET /auth/google/callback", handleGoogleCallback(oauthCfg, db))
	mux.HandleFunc("POST /auth/logout", handleLogout(db))
	mux.HandleFunc("POST /api/order", handleCreateOrder(db))
	mux.HandleFunc("POST /api/verify-payment", handleVerifyPayment(db))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func handleMeme(client anthropic.Client, rl *rateLimiter, db *sql.DB) http.HandlerFunc {
	secret := os.Getenv("SESSION_SECRET")
	return func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(clientIP(r)) {
			writeJSON(w, http.StatusTooManyRequests, memeResponse{Error: "Rate limit exceeded. Try again later."})
			return
		}

		// Logged-in path
		u := getSessionUser(r, db)
		if u != nil {
			if u.Credits <= 0 {
				writeJSON(w, http.StatusPaymentRequired, memeResponse{Error: "free_limit_reached"})
				return
			}
		} else {
			// Guest path
			count := readGuestCount(r, secret)
			if count >= 3 {
				writeJSON(w, http.StatusPaymentRequired, memeResponse{Error: "free_limit_reached"})
				return
			}
		}

		var req memeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, memeResponse{Error: "Invalid JSON"})
			return
		}
		req.Prompt = strings.TrimSpace(req.Prompt)
		if req.Prompt == "" {
			writeJSON(w, http.StatusBadRequest, memeResponse{Error: "Prompt is required"})
			return
		}
		log.Printf("meme request from %s: %q", clientIP(r), req.Prompt)

		choice, err := askClaude(r.Context(), client, req.Prompt)
		if err != nil {
			log.Printf("Claude error: %v", err)
			writeJSON(w, http.StatusInternalServerError, memeResponse{Error: "Meme machine broke. Try again."})
			return
		}

		imgURL, err := captionImgflip(choice)
		if err != nil {
			log.Printf("Imgflip error: %v", err)
			writeJSON(w, http.StatusInternalServerError, memeResponse{Error: "Meme machine broke. Try again."})
			return
		}

		var creditsLeft *int
		if u != nil {
			db.ExecContext(r.Context(), `UPDATE users SET credits = credits - 1 WHERE id = $1`, u.ID)
			remaining := u.Credits - 1
			creditsLeft = &remaining
		} else {
			count := readGuestCount(r, secret)
			writeGuestCount(w, secret, count+1)
		}

		writeJSON(w, http.StatusOK, memeResponse{MemeURL: imgURL, CreditsLeft: creditsLeft})
	}
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if !strings.HasPrefix(rawURL, "https://i.imgflip.com/") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	resp, err := http.Get(rawURL)
	if err != nil {
		http.Error(w, "fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Disposition", `attachment; filename="meme.jpg"`)
	io.Copy(w, resp.Body)
}

func askClaude(ctx context.Context, client anthropic.Client, prompt string) (*claudeChoice, error) {
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model("claude-haiku-4-5-20251001"),
			MaxTokens: 512,
			System: []anthropic.TextBlockParam{
				{Text: systemPrompt},
			},
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
			},
		})
		if err != nil {
			return nil, fmt.Errorf("claude: %w", err)
		}

		var text string
		for _, block := range resp.Content {
			if t, ok := block.AsAny().(anthropic.TextBlock); ok {
				text = strings.TrimSpace(t.Text)
				break
			}
		}
		if text == "" {
			if attempt == 0 {
				log.Printf("Empty Claude response, retrying")
				continue
			}
			return nil, fmt.Errorf("empty response from Claude")
		}

		text = extractJSON(text)

		var choice claudeChoice
		if err := json.Unmarshal([]byte(text), &choice); err != nil {
			if attempt == 0 {
				log.Printf("JSON parse failed (attempt 1), retrying: %v — raw: %s", err, text)
				continue
			}
			return nil, fmt.Errorf("parse JSON %w — raw: %s", err, text)
		}
		return &choice, nil
	}
	return nil, fmt.Errorf("failed after retries")
}

func captionImgflip(c *claudeChoice) (string, error) {
	username := os.Getenv("IMGFLIP_USERNAME")
	password := os.Getenv("IMGFLIP_PASSWORD")

	resp, err := http.PostForm("https://api.imgflip.com/caption_image", url.Values{
		"template_id": {c.TemplateID},
		"username":    {username},
		"password":    {password},
		"text0":       {c.TopText},
		"text1":       {c.BottomText},
	})
	if err != nil {
		return "", fmt.Errorf("imgflip http: %w", err)
	}
	defer resp.Body.Close()

	var result imgflipResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("imgflip decode: %w", err)
	}
	if !result.Success {
		return "", fmt.Errorf("imgflip: %s", result.ErrorMessage)
	}
	return result.Data.URL, nil
}

func extractJSON(s string) string {
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

func signValue(secret, value string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func readGuestCount(r *http.Request, secret string) int {
	cookie, err := r.Cookie("g")
	if err != nil {
		return 0
	}
	parts := strings.SplitN(cookie.Value, ".", 2)
	if len(parts) != 2 {
		return 0
	}
	if !hmac.Equal([]byte(signValue(secret, parts[0])), []byte(parts[1])) {
		return 0
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func writeGuestCount(w http.ResponseWriter, secret string, count int) {
	val := strconv.Itoa(count)
	http.SetCookie(w, &http.Cookie{
		Name:     "g",
		Value:    val + "." + signValue(secret, val),
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func handleFeedback(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req feedbackRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
			return
		}
		req.Message = strings.TrimSpace(req.Message)
		if req.Message == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Feedback cannot be empty"})
			return
		}
		if len(req.Message) > 1000 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Feedback must be under 1000 characters"})
			return
		}
		if _, err := db.ExecContext(r.Context(),
			`INSERT INTO feedback (message, ip) VALUES ($1, $2)`,
			req.Message, clientIP(r),
		); err != nil {
			log.Printf("feedback insert error: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to save feedback"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func generateID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func getSessionUser(r *http.Request, db *sql.DB) *user {
	cookie, err := r.Cookie("sid")
	if err != nil {
		return nil
	}
	var u user
	err = db.QueryRowContext(r.Context(), `
		SELECT u.id, u.email, u.name, u.credits
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id = $1 AND s.expires_at > NOW()
	`, cookie.Value).Scan(&u.ID, &u.Email, &u.Name, &u.Credits)
	if err != nil {
		return nil
	}
	return &u
}

func handleGoogleLogin(cfg *oauth2.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state := generateID()
		http.SetCookie(w, &http.Cookie{
			Name:     "oauth_state",
			Value:    state,
			Path:     "/",
			MaxAge:   300,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, cfg.AuthCodeURL(state, oauth2.AccessTypeOnline), http.StatusFound)
	}
}

func handleGoogleCallback(cfg *oauth2.Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stateCookie, err := r.Cookie("oauth_state")
		if err != nil || stateCookie.Value != r.URL.Query().Get("state") {
			http.Error(w, "invalid state", http.StatusBadRequest)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "oauth_state", MaxAge: -1, Path: "/"})

		token, err := cfg.Exchange(r.Context(), r.URL.Query().Get("code"))
		if err != nil {
			http.Error(w, "token exchange failed", http.StatusBadRequest)
			return
		}

		resp, err := cfg.Client(r.Context(), token).Get("https://www.googleapis.com/oauth2/v2/userinfo")
		if err != nil {
			http.Error(w, "userinfo failed", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		var info struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			http.Error(w, "userinfo decode failed", http.StatusInternalServerError)
			return
		}

		var userID int
		err = db.QueryRowContext(r.Context(), `
			INSERT INTO users (google_id, email, name)
			VALUES ($1, $2, $3)
			ON CONFLICT (google_id) DO UPDATE SET email = EXCLUDED.email, name = EXCLUDED.name
			RETURNING id
		`, info.ID, info.Email, info.Name).Scan(&userID)
		if err != nil {
			log.Printf("upsert user error: %v", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}

		sessionID := generateID()
		if _, err := db.ExecContext(r.Context(), `
			INSERT INTO sessions (id, user_id, expires_at) VALUES ($1, $2, NOW() + INTERVAL '30 days')
		`, sessionID, userID); err != nil {
			log.Printf("create session error: %v", err)
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     "sid",
			Value:    sessionID,
			Path:     "/",
			MaxAge:   30 * 24 * 3600,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

func handleMe(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := getSessionUser(r, db)
		if u == nil {
			writeJSON(w, http.StatusOK, map[string]any{"logged_in": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"logged_in": true,
			"name":      u.Name,
			"email":     u.Email,
			"credits":   u.Credits,
		})
	}
}

func handleLogout(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("sid")
		if err == nil {
			db.ExecContext(r.Context(), `DELETE FROM sessions WHERE id = $1`, cookie.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: "sid", MaxAge: -1, Path: "/"})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func handleCreateOrder(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := getSessionUser(r, db)
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login_required"})
			return
		}
		var req orderRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		p, ok := packs[req.Pack]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid pack"})
			return
		}
		client := razorpay.NewClient(os.Getenv("RAZORPAY_KEY_ID"), os.Getenv("RAZORPAY_KEY_SECRET"))
		order, err := client.Order.Create(map[string]any{
			"amount":          p.Amount,
			"currency":        "INR",
			"receipt":         generateID()[:16],
			"payment_capture": 1,
		}, nil)
		if err != nil {
			log.Printf("razorpay order error: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "payment init failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"order_id": order["id"],
			"amount":   p.Amount,
			"key_id":   os.Getenv("RAZORPAY_KEY_ID"),
			"name":     u.Name,
			"email":    u.Email,
		})
	}
}

func handleVerifyPayment(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := getSessionUser(r, db)
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login_required"})
			return
		}
		var req verifyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		p, ok := packs[req.Pack]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid pack"})
			return
		}

		// Verify Razorpay signature: HMAC-SHA256(order_id + "|" + payment_id, key_secret)
		mac := hmac.New(sha256.New, []byte(os.Getenv("RAZORPAY_KEY_SECRET")))
		mac.Write([]byte(req.OrderID + "|" + req.PaymentID))
		expected := hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(expected), []byte(req.Signature)) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid signature"})
			return
		}

		var newCredits int
		if err := db.QueryRowContext(r.Context(),
			`UPDATE users SET credits = credits + $1 WHERE id = $2 RETURNING credits`,
			p.Credits, u.ID,
		).Scan(&newCredits); err != nil {
			log.Printf("credit update error: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to add credits"})
			return
		}
		log.Printf("payment verified: user %d bought %s (+%d credits)", u.ID, req.Pack, p.Credits)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": newCredits})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
