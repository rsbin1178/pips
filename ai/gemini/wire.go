package gemini

import "github.com/rsbin/pips/ai"

// Gemini content roles (the wire has only these two).
const (
	roleUser  = "user"
	roleModel = "model"
)

// Gemini wire types — the subset this adapter produces and consumes.

type generateRequest struct {
	Contents          []wireContent     `json:"contents"`
	SystemInstruction *wireContent      `json:"systemInstruction,omitempty"`
	CachedContent     string            `json:"cachedContent,omitempty"`
	Tools             []wireTool        `json:"tools,omitempty"`
	ToolConfig        *wireToolConfig   `json:"toolConfig,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type wireContent struct {
	Role  string     `json:"role,omitempty"` // "user" | "model"
	Parts []wirePart `json:"parts"`
}

// wirePart is a content part. Exactly one payload field is set; thought and
// thoughtSignature are metadata that may accompany any of them.
type wirePart struct {
	Text             string            `json:"text,omitempty"`
	InlineData       *wireBlob         `json:"inlineData,omitempty"`
	FileData         *wireFileData     `json:"fileData,omitempty"`
	FunctionCall     *wireFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *wireFunctionResp `json:"functionResponse,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
}

type wireBlob struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"` // base64
}

type wireFileData struct {
	MIMEType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

type wireFunctionCall struct {
	ID   string  `json:"id,omitempty"`
	Name string  `json:"name"`
	Args ai.JSON `json:"args,omitempty"`
}

type wireFunctionResp struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type wireTool struct {
	FunctionDeclarations []wireFunctionDecl `json:"functionDeclarations,omitempty"`
}

type wireFunctionDecl struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Parameters  *ai.Schema `json:"parameters,omitempty"`
}

type wireToolConfig struct {
	FunctionCallingConfig *wireFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type wireFunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"` // AUTO | ANY | NONE
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type generationConfig struct {
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"topP,omitempty"`
	TopK               *int            `json:"topK,omitempty"`
	Seed               *int64          `json:"seed,omitempty"`
	FrequencyPenalty   *float64        `json:"frequencyPenalty,omitempty"`
	PresencePenalty    *float64        `json:"presencePenalty,omitempty"`
	ResponseLogProbs   *bool           `json:"responseLogprobs,omitempty"`
	LogProbs           *int            `json:"logprobs,omitempty"`
	MaxOutputTokens    *int            `json:"maxOutputTokens,omitempty"`
	StopSequences      []string        `json:"stopSequences,omitempty"`
	ResponseMIMEType   string          `json:"responseMimeType,omitempty"`
	ResponseSchema     *ai.Schema      `json:"responseSchema,omitempty"`
	ResponseModalities []string        `json:"responseModalities,omitempty"`
	ThinkingConfig     *thinkingConfig `json:"thinkingConfig,omitempty"`
}

type thinkingConfig struct {
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
}

type generateResponse struct {
	Candidates    []wireCandidate `json:"candidates"`
	UsageMetadata *wireUsage      `json:"usageMetadata"`
	ModelVersion  string          `json:"modelVersion"`
	ResponseID    string          `json:"responseId"`
}

type wireCandidate struct {
	Content      wireContent `json:"content"`
	FinishReason string      `json:"finishReason"`
	Index        int         `json:"index"`
}

type wireUsage struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
}
