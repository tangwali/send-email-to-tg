package telegram

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
)

type Client struct {
	Token  string
	ChatID string
}

func New(token, chatID string) *Client {
	return &Client{Token: token, ChatID: chatID}
}

func (c *Client) SendHTML(html string) error {
	api := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", c.Token)
	form := url.Values{}
	form.Set("chat_id", c.ChatID)
	form.Set("text", html)
	form.Set("parse_mode", "HTML")
	form.Set("disable_web_page_preview", "true")

	resp, err := http.PostForm(api, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram %s: %s", resp.Status, string(b))
	}
	return nil
}
