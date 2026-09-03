package store

type Account struct {
	Email                 string `json:"email"`
	ChatGPTAccountID      string `json:"account_id"`
	PlanType              string `json:"plan_type"`
	AccessTokenEncrypted  string `json:"-"`
	RefreshTokenEncrypted string `json:"-"`
	IDTokenEncrypted      string `json:"-"`
	ExpiresAt             int64  `json:"expires_at"`
	UpdatedAt             int64  `json:"updated_at"`
}

type APIKey struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Hash       string `json:"-"`
	Prefix     string `json:"prefix"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt *int64 `json:"last_used_at"`
	RevokedAt  *int64 `json:"revoked_at"`
}

type RequestLog struct {
	ID           string `json:"id"`
	RequestID    string `json:"request_id"`
	APIKeyID     string `json:"api_key_id,omitempty"`
	APIKeyName   string `json:"api_key_name,omitempty"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	Model        string `json:"model,omitempty"`
	Status       int    `json:"status"`
	DurationMS   int64  `json:"duration_ms"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Error        string `json:"error,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

type TodayStats struct {
	Requests     int64   `json:"requests"`
	Successes    int64   `json:"successes"`
	SuccessRate  float64 `json:"success_rate"`
	AverageMS    float64 `json:"average_ms"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

type QuotaSnapshot struct {
	Payload   string `json:"payload"`
	FetchedAt int64  `json:"fetched_at"`
}
