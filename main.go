package main

import (
	_ "embed"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
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
264917984 - Hard To Swallow Pills: use for delivering a truth people don't want to hear
309868304 - Woman Yelling at Cat: use for overreaction, someone dramatic vs something completely unbothered
438680 - Batman Slapping Robin: use for correcting someone mid-sentence, shutting down a bad take
100777631 - Is This A Pigeon: use for confidently misidentifying something, wrong assumption played straight
91538330 - Expanding Brain: use for escalating ideas from sensible to galaxy brain
80707627 - Sad Pablo Escobar: use for alone and waiting, boredom or abandonment
193426690 - Waiting Skeleton: use for waiting so long you've turned to bones, extreme delays
222403160 - Bernie I Am Once Again Asking: use for repeatedly asking for the same thing and never getting it
178591752 - Me and the Boys: use for a group doing something chaotic or unified
135256802 - Epic Handshake: use for two unlikely things agreeing on something unexpected
196514422 - Blank Nut Button: use for can't help doing something even though you shouldn't
101470 - Ancient Aliens Guy: use for wildly blaming something inexplicable on a single absurd cause
61579 - One Does Not Simply: use for something that sounds simple but is actually impossible
14371066 - Yoda: use for wise reversal, stating something in the wrong order for comic effect
4087833 - Waiting for Gf: use for patient waiting, tolerating something unreasonably long
79132341 - Bike Fall: use for self-sabotage, person causing their own problem while someone watches
252600902 - Always Has Been: use for revealing something was always true, the astronaut meme
161865971 - Tuxedo Winnie the Pooh: use for basic thing vs unnecessarily fancy version of the same thing
188390779 - Monkey Puppet: use for side-eye awkward look, slowly turning away from something uncomfortable
322841258 - Among Us Drip: use for normal thing vs same thing but confident and dripped out
20007896 - Oprah You Get A: use for giving the same thing to everyone indiscriminately, chaos mode

Rules for captions:
- Keep top and bottom text short — under 8 words each
- Be specific to the situation, never generic
- Punch line goes on the bottom
- If the template only needs one caption put the joke on bottom and leave top empty
- Never explain the joke`

type memeRequest struct {
	Prompt string `json:"prompt"`
}

type memeResponse struct {
	MemeURL string `json:"meme_url,omitempty"`
	Error   string `json:"error,omitempty"`
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
	for _, v := range []string{"ANTHROPIC_API_KEY", "IMGFLIP_USERNAME", "IMGFLIP_PASSWORD"} {
		if os.Getenv(v) == "" {
			log.Fatalf("Required environment variable %s is not set", v)
		}
	}

	client := anthropic.NewClient()
	rl := newRateLimiter()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", serveIndex)
	mux.HandleFunc("POST /api/meme", handleMeme(client, rl))
	mux.HandleFunc("GET /api/download", handleDownload)

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

func handleMeme(client anthropic.Client, rl *rateLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(clientIP(r)) {
			writeJSON(w, http.StatusTooManyRequests, memeResponse{Error: "Rate limit exceeded. Try again later."})
			return
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

		writeJSON(w, http.StatusOK, memeResponse{MemeURL: imgURL})
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
