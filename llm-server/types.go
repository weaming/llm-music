package main

type chatRequest struct {
	SystemPrompt string `json:"system_prompt"`
	UserPrompt   string `json:"user_prompt"`
}

type chatResponse struct {
	Content string `json:"content"`
	Error   string `json:"error"`
}
