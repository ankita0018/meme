# blurtit.lol

> Describe your pain. We'll make a meme.

A meme generator where you type a situation in plain English and get a captioned meme back in seconds — powered by Claude AI and Imgflip. Built for monetisation from day one with a free tier, Google login, and Razorpay credit packs.

---

## Table of Contents

1. [Architecture Overview](#architecture-overview)
2. [Tech Stack](#tech-stack)
3. [How Meme Generation Works](#how-meme-generation-works)
4. [User Journey & Free Tier](#user-journey--free-tier)
5. [Authentication — Google OAuth](#authentication--google-oauth)
6. [Credit System](#credit-system)
7. [Payments — Razorpay](#payments--razorpay)
8. [Feedback System](#feedback-system)
9. [Download & Share](#download--share)
10. [Database Schema](#database-schema)
11. [API Reference](#api-reference)
12. [Environment Variables](#environment-variables)
13. [Running Locally](#running-locally)
14. [Deploying to Fly.io](#deploying-to-flyio)

---

## Architecture Overview

```
┌─────────────────────────────────────────────────────────┐
│                        Browser                          │
│  index.html (single page — HTML + CSS + vanilla JS)     │
└────────────────────────┬────────────────────────────────┘
                         │ HTTP
┌────────────────────────▼────────────────────────────────┐
│              Go HTTP Server (net/http)                  │
│                                                         │
│  GET  /                 → serve index.html (embedded)   │
│  POST /api/meme         → generate meme                 │
│  GET  /api/download     → proxy image download          │
│  GET  /api/me           → current user info             │
│  POST /api/feedback     → store feedback                │
│  POST /api/order        → create Razorpay order         │
│  POST /api/verify-payment → verify & credit user        │
│  GET  /auth/google      → start OAuth flow              │
│  GET  /auth/google/callback → finish OAuth flow         │
│  POST /auth/logout      → clear session                 │
└────┬──────────┬─────────────┬──────────────────┬────────┘
     │          │             │                  │
     ▼          ▼             ▼                  ▼
 Claude AI   Imgflip      Supabase          Razorpay
 (Haiku)     (captions)   (Postgres)        (payments)
```

Everything is a single Go binary. `index.html` is embedded at compile time via `//go:embed`, so there are no static file servers or CDNs — one process handles everything.

---

## Tech Stack

| Layer | Technology | Why |
|---|---|---|
| Language | Go 1.25 | Fast, single binary, great stdlib HTTP |
| Frontend | Vanilla HTML/CSS/JS | No build step, instant load |
| AI | Claude Haiku 4.5 | Fast + cheap for JSON-only output |
| Meme rendering | Imgflip API | Pre-built templates, free tier |
| Database | PostgreSQL (Supabase) | Scales horizontally, managed |
| Auth | Google OAuth 2.0 | One-click, no password management |
| Payments | Razorpay | Best UPI support in India |
| Hosting | Fly.io | Global edge, auto-scaling |

---

## How Meme Generation Works

This is the core feature. Here's the full internal flow:

```
User types: "my manager scheduling a meeting at 5:59pm"
                        │
                        ▼
         POST /api/meme { prompt: "..." }
                        │
              ┌─────────▼──────────┐
              │   Check credits    │
              │  (guest OR login)  │
              └─────────┬──────────┘
                        │ allowed
                        ▼
         ┌──────────────────────────────┐
         │        Claude Haiku          │
         │                              │
         │  System prompt: 30 meme      │
         │  templates with descriptions │
         │  + caption rules             │
         │                              │
         │  User: "my manager..."       │
         │                              │
         │  Response (JSON only):       │
         │  {                           │
         │    "template_id": "4087833", │
         │    "top_text": "me at 5:58", │
         │    "bottom_text": "meeting   │
         │     invite lands"            │
         │  }                           │
         └──────────┬───────────────────┘
                    │
                    ▼
         ┌──────────────────────────────┐
         │        Imgflip API           │
         │                              │
         │  POST /caption_image         │
         │  template_id + text0 + text1 │
         │                              │
         │  Returns: image URL          │
         │  https://i.imgflip.com/xxx   │
         └──────────┬───────────────────┘
                    │
                    ▼
         { meme_url: "https://i.imgflip.com/xxx" }
```

### Claude's role in detail

Claude receives a **system prompt** listing 30 meme templates, each with an ID and a precise description of when to use it:

```
181913649 - Drake approving/rejecting: use for preference comparison...
87743020  - Two Buttons: use when someone is sweating between two choices...
...
```

It responds with **only JSON** — no explanation, no markdown. The response is parsed and fed directly to Imgflip. If Claude returns malformed JSON, the request is retried once automatically.

### Why Haiku, not Sonnet?

The task is pure template selection + short text — it doesn't need reasoning. Haiku does it correctly at ~10x lower cost and 2x faster response time.

---

## User Journey & Free Tier

```
New visitor
     │
     ▼
Opens blurtit.lol
     │
     ├── Meme #1 ──► Cookie: g = "1.<hmac>"
     ├── Meme #2 ──► Cookie: g = "2.<hmac>"
     ├── Meme #3 ──► Cookie: g = "3.<hmac>"
     │
     ▼
4th attempt
     │
     ▼
Backend returns HTTP 402
     │
     ▼
Paywall modal appears
     │
     ├─── "Sign in with Google → 7 more free"
     │                │
     │                ▼
     │           Google OAuth
     │                │
     │                ▼
     │         7 credits in DB
     │
     └─── "Buy a pack from ₹9"
                      │
                      ▼
               Pack picker UI
```

### How the guest cookie works internally

The guest counter is stored in a **signed cookie** named `g`:

```
Cookie value:  "3.a7f9c2d1e4b8..."
               └─┘ └────────────┘
               count  HMAC-SHA256(count, SESSION_SECRET)
```

On every meme request, the server:
1. Reads the `g` cookie
2. Splits on `.` → `count` and `signature`
3. Recomputes `HMAC-SHA256(count, SESSION_SECRET)`
4. Compares with constant-time `hmac.Equal` to prevent timing attacks
5. Rejects if mismatch (tampered cookie → treated as 0)

This means users cannot increment to a fake number by editing the cookie — the HMAC would not match.

---

## Authentication — Google OAuth

```
User clicks "Continue with Google"
              │
              ▼
   GET /auth/google
              │
   Generate random state (32 bytes hex)
   Store in short-lived cookie (5 min TTL)
              │
              ▼
   Redirect to Google:
   accounts.google.com/o/oauth2/auth
   ?client_id=...
   &redirect_uri=.../auth/google/callback
   &scope=openid email profile
   &state=<random>
              │
        User approves
              │
              ▼
   GET /auth/google/callback?code=...&state=...
              │
   Verify state matches cookie (CSRF protection)
              │
   Exchange code → access token (server-to-server)
              │
   Fetch user info:
   GET googleapis.com/oauth2/v2/userinfo
   → { id, email, name }
              │
   UPSERT into users table:
   INSERT ... ON CONFLICT (google_id) DO UPDATE
              │
   Create session:
   INSERT INTO sessions (id=random64hex, user_id, expires_at=+30days)
              │
   Set cookie: sid=<session_id> (HttpOnly, SameSite=Lax)
              │
              ▼
   Redirect → /
```

### Why state parameter?

The `state` value is a random string generated per login attempt and stored in a cookie. When Google redirects back, the server checks that the `state` in the URL matches the cookie. This prevents **CSRF attacks** where a malicious site tricks a user into authenticating with someone else's account.

### Session cookie security flags

```
HttpOnly  → JS cannot read it (blocks XSS token theft)
SameSite=Lax → not sent on cross-site POST requests (CSRF protection)
MaxAge=30days → persistent login
Path=/    → sent on all routes
```

---

## Credit System

Credits are stored on the `users` table. Every meme request goes through this decision tree:

```
POST /api/meme
      │
      ├─ sid cookie present?
      │         │
      │    ┌────▼────┐
      │    │ Look up │
      │    │ session │
      │    │  in DB  │
      │    └────┬────┘
      │         │
      │    Valid session?
      │    ┌────┴────────────────┐
      │    │ YES                 │ NO
      │    ▼                     ▼
      │  Check user.credits    Guest cookie path
      │  > 0?                  (count < 3?)
      │  ┌──┴──┐
      │  │ YES │ NO → HTTP 402
      │  ▼
      Generate meme
      UPDATE users SET credits = credits - 1
      Return { meme_url, credits_left }
```

The `credits_left` value is returned in every meme response so the frontend can update the counter in real time without a separate API call.

---

## Payments — Razorpay

### Pack options

| Pack | Price | Credits | Per meme |
|---|---|---|---|
| Starter | ₹9 | 10 | ₹0.90 |
| Popular | ₹19 | 25 | ₹0.76 |
| Stash | ₹49 | 75 | ₹0.65 |

### Full payment flow

```
User selects pack → clicks "Pay ₹19"
              │
              ▼
   POST /api/order { pack: "popular" }
              │
   Server calls Razorpay API:
   POST api.razorpay.com/v1/orders
   { amount: 1900, currency: "INR" }
              │
   Returns: { order_id: "order_xxx", amount: 1900, key_id }
              │
              ▼
   Frontend opens Razorpay modal:
   new Razorpay({ key, amount, order_id, ... })
              │
        User pays via
        UPI / Card / Netbanking
              │
              ▼
   Razorpay calls handler with:
   { razorpay_payment_id,
     razorpay_order_id,
     razorpay_signature }
              │
              ▼
   POST /api/verify-payment
              │
   Server verifies:
   expected = HMAC-SHA256(
     order_id + "|" + payment_id,
     RAZORPAY_KEY_SECRET
   )
   expected == signature?  ──NO──► HTTP 400
              │ YES
              ▼
   UPDATE users SET credits = credits + 25
   Returns { ok: true, credits: 32 }
              │
              ▼
   UI updates credit count, closes modal
```

### Why signature verification matters

Without verifying the signature on the backend, a user could:
1. Open the Razorpay modal
2. Dismiss it without paying
3. Forge a successful payment response in the browser console
4. Send fake IDs to `/api/verify-payment`

The HMAC signature is computed using `RAZORPAY_KEY_SECRET` which **never leaves the server**. A forged payload would fail the signature check.

---

## Feedback System

A minimal form at the bottom of the page. Submissions go to `POST /api/feedback` which inserts into the `feedback` table with the message and submitter IP.

View all feedback in Supabase: **Table Editor → feedback**.

---

## Download & Share

### Download

The browser can't force-download a cross-origin image with a simple `<a>` tag. The Go backend proxies the image:

```
Browser                     Go server              Imgflip
   │                            │                     │
   │── GET /api/download ───────►│                     │
   │   ?url=https://i.imgflip.. │                     │
   │                            │── GET image ────────►│
   │                            │◄─ image bytes ───────│
   │◄─ image bytes ─────────────│                     │
   │   Content-Disposition:     │                     │
   │   attachment; filename=    │                     │
   │   meme.jpg                 │                     │
```

The server validates the URL starts with `https://i.imgflip.com/` before proxying to prevent the endpoint being used as an open proxy.

### Share

The share button builds a URL with the meme embedded as a query parameter:

```
https://blurtit.lol/?meme=https%3A%2F%2Fi.imgflip.com%2Fxxx
```

On page load, the frontend checks for `?meme=` and if present, displays that meme immediately. On mobile, `navigator.share()` opens the native share sheet. On desktop it copies to clipboard with a toast.

---

## Database Schema

```sql
-- Stores registered users (created on first Google sign-in)
CREATE TABLE users (
    id         SERIAL PRIMARY KEY,
    google_id  TEXT UNIQUE NOT NULL,  -- Google's user ID
    email      TEXT NOT NULL,
    name       TEXT,
    credits    INT NOT NULL DEFAULT 7, -- 7 free on sign-up
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Login sessions (30-day TTL)
CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,       -- random 64-char hex
    user_id    INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL
);

-- User feedback submissions
CREATE TABLE feedback (
    id         SERIAL PRIMARY KEY,
    message    TEXT NOT NULL,
    ip         TEXT,                   -- submitter IP
    created_at TIMESTAMPTZ DEFAULT NOW()
);
```

---

## API Reference

| Method | Path | Auth | Description |
|---|---|---|---|
| `GET` | `/` | — | Serve the app |
| `POST` | `/api/meme` | optional | Generate a meme |
| `GET` | `/api/download` | — | Proxy-download meme image |
| `GET` | `/api/me` | optional | Current user info |
| `POST` | `/api/feedback` | — | Submit feedback |
| `POST` | `/api/order` | required | Create Razorpay order |
| `POST` | `/api/verify-payment` | required | Verify payment & add credits |
| `GET` | `/auth/google` | — | Start Google OAuth |
| `GET` | `/auth/google/callback` | — | OAuth callback |
| `POST` | `/auth/logout` | — | Clear session |

### `POST /api/meme`
```json
// Request
{ "prompt": "my manager scheduling a meeting at 5:59pm" }

// Response 200
{ "meme_url": "https://i.imgflip.com/xxx.jpg", "credits_left": 6 }

// Response 402 (limit reached)
{ "error": "free_limit_reached" }
```

### `POST /api/order`
```json
// Request
{ "pack": "popular" }  // "starter" | "popular" | "stash"

// Response 200
{ "order_id": "order_xxx", "amount": 1900, "key_id": "rzp_...", "name": "...", "email": "..." }
```

### `POST /api/verify-payment`
```json
// Request
{
  "order_id": "order_xxx",
  "payment_id": "pay_xxx",
  "signature": "hmac_hex",
  "pack": "popular"
}

// Response 200
{ "ok": true, "credits": 32 }
```

---

## Environment Variables

| Variable | Description |
|---|---|
| `ANTHROPIC_API_KEY` | Claude API key |
| `IMGFLIP_USERNAME` | Imgflip account username |
| `IMGFLIP_PASSWORD` | Imgflip account password |
| `DATABASE_URL` | Postgres connection string (URL-encoded password) |
| `SESSION_SECRET` | 32-byte hex string for HMAC cookie signing |
| `GOOGLE_CLIENT_ID` | Google OAuth client ID |
| `GOOGLE_CLIENT_SECRET` | Google OAuth client secret |
| `BASE_URL` | App's public URL (e.g. `https://blurtit.lol`) |
| `RAZORPAY_KEY_ID` | Razorpay Key ID (`rzp_test_...` or `rzp_live_...`) |
| `RAZORPAY_KEY_SECRET` | Razorpay Key Secret |

---

## Running Locally

```sh
# 1. Clone and install dependencies
git clone ...
cd meme
go mod download

# 2. Fill in your .env file
cp .env.example .env   # edit values

# 3. Load env and run
export $(cat .env | xargs) && go run .

# App is at http://localhost:8080
```

---

## Deploying to Fly.io

```sh
# First deploy
fly auth login
fly secrets set ANTHROPIC_API_KEY="..." IMGFLIP_USERNAME="..." # all vars
fly deploy

# Subsequent deploys
fly deploy
```

Add both redirect URIs in Google Cloud Console → OAuth client:
- `http://localhost:8080/auth/google/callback` (local)
- `https://blurtit-lol.fly.dev/auth/google/callback` (production)
