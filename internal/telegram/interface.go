// internal/telegram/interface.go
package telegram

import (
	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// BotInterface defines the methods required for Telegram bot interactions.
type BotInterface interface {
	Send(c tgbotapi.Chattable) (tgbotapi.Message, error)
	Request(c tgbotapi.Chattable) (*tgbotapi.APIResponse, error)
}