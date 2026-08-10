// Package agnesback implements the lark-agnes-back backend.
//
// Unlike the CLI backends (claude/miniagent), it does NOT fork a subprocess.
// It registers as backendType "agnes" over SSE and, when a bound chat sends a
// slash command, calls the Agnes AI REST API directly (image/video prompt
// generation via the chat-completions model, and image/video generation via
// the dedicated async models) and reports the result back as a Notice/Result
// Control. Image bytes are downloaded and shipped as a TypeFile Control so the
// frontend uploads them into the chat.
package agnesback

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/config"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// controlSender is the subset of *backendrpc.Client the handler needs to POST
// a Control. Lifted as an interface so tests inject a fake without a live SSE
// connection. *Client satisfies it via SendControl.
type controlSender = backendrpc.ControlSender

// APIClient is the subset of *Client the handler calls. The concrete *Client
// (client.go) satisfies it; tests inject a fake.
type APIClient interface {
	GeneratePrompt(ctx context.Context, systemPrompt, userText string) (string, error)
	GenerateImage(ctx context.Context, prompt string) ([]byte, string, error)
	GenerateVideo(ctx context.Context, prompt string, pollCB func(status string, progress int)) (string, error)
}

// jobTimeout bounds one /image or /video call (the API call itself plus, for
// /video, the poll loop). Image gen takes seconds to tens of seconds; video gen
// can take minutes. 10 minutes absorbs slow polls without hanging forever.
const jobTimeout = 10 * time.Minute

// noticeTimeout bounds a single Notice/Progress POST. Kept short so a wedged
// frontend does not stall the handler long.
const noticeTimeout = 10 * time.Second

// Config is the resolved Agnes settings shared by the handler and the client.
// It is an alias for ClientConfig so the handler and client agree on one shape.
type Config = ClientConfig

// Handler owns the Agnes client and emits results/notices back to the frontend.
// Each command runs asynchronously on its own goroutine (bridgebase.GoSafe) so
// the SSE event loop never blocks on a multi-minute video poll.
type Handler struct {
	cfg    Config
	client APIClient
	rpc    controlSender
	logger *log.Logger
}

// NewHandler wires the handler. rpc is typically a *backendrpc.Client.
func NewHandler(cfg Config, client APIClient, rpc controlSender, logger *log.Logger) *Handler {
	if logger == nil {
		logger = log.Nop()
	}
	return &Handler{cfg: cfg, client: client, rpc: rpc, logger: logger}
}

// FromConfig builds a Config + real Client from config.AgnesBack, ready for New.
// It maps the config struct (defaults already applied by config.Load) onto
// ClientConfig fields. logger is optional (defaults to Nop).
func FromConfig(c config.AgnesBack, logger *log.Logger) (Config, *Client) {
	cc := ClientConfig{
		BaseURL:    c.BaseURL,
		APIKey:     c.APIKey,
		ChatModel:  c.ChatModel,
		ImageModel: c.ImageModel,
		VideoModel: c.VideoModel,
		ImageSize:  c.ImageSize,
		ImageRatio: c.ImageRatio,
	}
	return cc, New(cc, logger)
}

// HandleEvent dispatches Prompt events to the command handlers. Unknown
// commands and empty args surface as a terminal notice bound to promptID so the
// frontend finalises the progress card instead of leaving it spinning.
func (h *Handler) HandleEvent(ctx context.Context, ev *protocol.Event) error {
	if ev.Type != protocol.TypePrompt || ev.Prompt == nil {
		return nil
	}
	chatID := ev.Prompt.ChatID
	promptID := ev.PromptID
	cardMsgID := ev.Prompt.CardMessageID
	prompt := strings.TrimSpace(ev.Prompt.Text)

	h.logger.Info("agnes: handle event",
		"chat_id", chatID,
		"prompt_id", promptID,
		"card_msg_id", cardMsgID,
		"prompt", truncateString(prompt, 100))
	cmd, arg := splitCommand(prompt)
	switch cmd {
	case "/image-prompt":
		if arg == "" {
			h.logger.Info("agnes: image-prompt missing args", "chat_id", chatID)
			return h.notify(ctx, chatID, promptID, cardMsgID, "error", "用法",
				"用法：/image-prompt <图片描述>\n例如：/image-prompt 一只在雨中漫步的橘猫")
		}
		h.logger.Info("agnes: image-prompt job start",
			"chat_id", chatID,
			"prompt_id", promptID,
			"arg", truncateString(arg, 100))
		h.runJob(chatID, promptID, cardMsgID, "图片提示词", func(c context.Context) error {
			return h.handleImagePrompt(c, chatID, promptID, arg)
		})
	case "/image":
		if arg == "" {
			h.logger.Info("agnes: image missing args", "chat_id", chatID)
			return h.notify(ctx, chatID, promptID, cardMsgID, "error", "用法",
				"用法：/image <提示词>\n例如：/image A luminous floating city above a misty canyon")
		}
		h.logger.Info("agnes: image job start",
			"chat_id", chatID,
			"prompt_id", promptID,
			"prompt", truncateString(arg, 100))
		h.runJob(chatID, promptID, cardMsgID, "图片生成", func(c context.Context) error {
			return h.handleImage(c, chatID, promptID, cardMsgID, arg)
		})
	case "/video-prompt":
		if arg == "" {
			h.logger.Info("agnes: video-prompt missing args", "chat_id", chatID)
			return h.notify(ctx, chatID, promptID, cardMsgID, "error", "用法",
				"用法：/video-prompt <视频描述>\n例如：/video-prompt 猫咪在海滩上漫步的日落场景")
		}
		h.logger.Info("agnes: video-prompt job start",
			"chat_id", chatID,
			"prompt_id", promptID,
			"arg", truncateString(arg, 100))
		h.runJob(chatID, promptID, cardMsgID, "视频提示词", func(c context.Context) error {
			return h.handleVideoPrompt(c, chatID, promptID, arg)
		})
	case "/video":
		if arg == "" {
			h.logger.Info("agnes: video missing args", "chat_id", chatID)
			return h.notify(ctx, chatID, promptID, cardMsgID, "error", "用法",
				"用法：/video <提示词>\n例如：/video A cinematic shot of a cat walking on the beach")
		}
		h.logger.Info("agnes: video job start",
			"chat_id", chatID,
			"prompt_id", promptID,
			"prompt", truncateString(arg, 100))
		h.runJob(chatID, promptID, cardMsgID, "视频生成", func(c context.Context) error {
			return h.handleVideo(c, chatID, promptID, cardMsgID, arg)
		})
	case "/agnes-help", "":
		return h.notify(ctx, chatID, promptID, cardMsgID, "info", "Agnes 用法", helpText)
	default:
		return h.notify(ctx, chatID, promptID, cardMsgID, "warning", "未知指令",
			"未识别的指令："+cmd+"。发送 /agnes-help 查看可用指令。")
	}
	return nil
}

// runJob launches fn on its own goroutine with a jobTimeout-bound context. The
// SSE loop returns immediately. The job emits its own terminal notice so the
// progress card is always finalised. GoSafe recovers panics so a bad API
// response cannot crash the backend.
func (h *Handler) runJob(chatID, promptID, cardMsgID, label string, fn func(context.Context) error) {
	progCtx, cancel := context.WithTimeout(context.Background(), noticeTimeout)
	defer cancel()
	if err := h.notifyProgress(progCtx, chatID, promptID, "⏳ "+label+"执行中…"); err != nil {
		h.logger.Warn("progress banner failed", log.FieldChatID, chatID, log.FieldError, err)
	}
	bridgebase.GoSafe(h.logger, "agnes job: "+label, func() {
		ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
		defer cancel()
		if err := fn(ctx); err != nil {
			h.logger.Error(label+" failed", log.FieldChatID, chatID, log.FieldError, err)
			nctx, ncancel := context.WithTimeout(context.Background(), noticeTimeout)
			defer ncancel()
			if nerr := h.notify(nctx, chatID, promptID, cardMsgID, "error", label+"失败", err.Error()); nerr != nil {
				h.logger.Warn(label+" error-notice failed", log.FieldChatID, chatID, log.FieldError, nerr)
			}
		}
	})
}

// handleImagePrompt calls the chat model to expand a terse description into a
// full image-generation prompt, then returns it as a terminal result.
func (h *Handler) handleImagePrompt(ctx context.Context, chatID, promptID, desc string) error {
	h.logger.Info("agnes: handle image prompt start",
		"chat_id", chatID,
		"prompt_id", promptID,
		"desc", truncateString(desc, 100))
	text, err := h.client.GeneratePrompt(ctx, imagePromptSystem, desc)
	if err != nil {
		return err
	}
	h.logger.Info("agnes: handle image prompt success",
		"chat_id", chatID,
		"prompt_id", promptID,
		"result", truncateString(text, 200))
	return h.notify(ctx, chatID, promptID, "", "success", "图片提示词", text)
}

// handleVideoPrompt calls the chat model to expand a description into a
// full video-generation prompt.
func (h *Handler) handleVideoPrompt(ctx context.Context, chatID, promptID, desc string) error {
	h.logger.Info("agnes: handle video prompt start",
		"chat_id", chatID,
		"prompt_id", promptID,
		"desc", truncateString(desc, 100))
	text, err := h.client.GeneratePrompt(ctx, videoPromptSystem, desc)
	if err != nil {
		return err
	}
	h.logger.Info("agnes: handle video prompt success",
		"chat_id", chatID,
		"prompt_id", promptID,
		"result", truncateString(text, 200))
	return h.notify(ctx, chatID, promptID, "", "success", "视频提示词", text)
}

// handleImage generates an image and ships it as a TypeFile Control. The
// frontend base64-decodes the payload and uploads it into the chat. The image
// API returns a URL by default; GenerateImage downloads the bytes internally so
// the chat gets an inline image, not a bare link.
func (h *Handler) handleImage(ctx context.Context, chatID, promptID, cardMsgID, prompt string) error {
	h.logger.Info("agnes: handle image start",
		"chat_id", chatID,
		"prompt_id", promptID,
		"prompt", truncateString(prompt, 100))
	data, mime, err := h.client.GenerateImage(ctx, prompt)
	if err != nil {
		return err
	}
	nctx, cancel := context.WithTimeout(context.Background(), noticeTimeout)
	defer cancel()
	h.logger.Info("agnes: sending file control",
		"chat_id", chatID,
		"prompt_id", promptID,
		"card_msg_id", cardMsgID,
		"bytes", len(data),
		"mime", mime)
	if err := h.rpc.SendControl(nctx, &protocol.Control{
		Type:     protocol.TypeFile,
		PromptID: promptID,
		ChatID:   chatID,
		File: &protocol.FilePayload{
			ChatID:          chatID,
			FileName:        "agnes-image.png",
			MIMEType:        mime,
			Content:         base64.StdEncoding.EncodeToString(data),
			UpdateMessageID: "", // 最终结果发新卡，避免更新卡片回弹
		},
	}); err != nil {
		h.logger.Error("agnes: failed to send image file control",
			"chat_id", chatID,
			"prompt_id", promptID,
			"card_msg_id", cardMsgID,
			"error", err)
		return err
	}
	return nil
}

// handleVideo creates a video task, polls it to completion, and posts the final
// video URL as a terminal notice. Video files are large and often exceed the
// Feishu 30 MiB file cap, so we surface the URL rather than shipping bytes.
// Progress is reflected on the command's own progress card after each poll.
func (h *Handler) handleVideo(ctx context.Context, chatID, promptID, cardMsgID, prompt string) error {
	h.logger.Info("agnes: handle video start",
		"chat_id", chatID,
		"prompt_id", promptID,
		"prompt", truncateString(prompt, 100))
	progress := func(status string, pct int) {
		pctx, cancel := context.WithTimeout(context.Background(), noticeTimeout)
		defer cancel()
		msg := fmt.Sprintf("视频生成中…（%s %d%%）", status, pct)
		h.logger.Debug("agnes: video progress",
			"chat_id", chatID,
			"prompt_id", promptID,
			"status", status,
			"progress", pct)
		_ = h.notifyProgress(pctx, chatID, promptID, msg)
	}
	url, err := h.client.GenerateVideo(ctx, prompt, progress)
	if err != nil {
		return err
	}
	nctx, cancel := context.WithTimeout(context.Background(), noticeTimeout)
	defer cancel()
	h.logger.Info("agnes: handle video success",
		"chat_id", chatID,
		"prompt_id", promptID,
		"card_msg_id", cardMsgID,
		"url", url)
	if err := h.notify(nctx, chatID, promptID, "", "success", "视频生成完成", url); err != nil {
		h.logger.Error("agnes: failed to send video result notice",
			"chat_id", chatID,
			"prompt_id", promptID,
			"card_msg_id", cardMsgID,
			"error", err)
		return err
	}
	return nil
}

// --- emit helpers ---

func (h *Handler) notify(ctx context.Context, chatID, promptID, cardMsgID, level, title, message string) error {
	h.logger.Info("agnes: notify called",
		"chat_id", chatID,
		"prompt_id", promptID,
		"card_msg_id", cardMsgID,
		"level", level,
		"title", title,
		"message", truncateString(message, 100))
	if chatID == "" {
		return fmt.Errorf("notify: chatID is empty")
	}
	h.logger.Info("agnes: sending notice",
		"chat_id", chatID,
		"prompt_id", promptID,
		"card_msg_id", cardMsgID)
	err := h.rpc.SendControl(ctx, &protocol.Control{
		Type:     protocol.TypeNotice,
		PromptID: promptID,
		ChatID:   chatID,
		Notice: &protocol.NoticePayload{
			Level:           level,
			Title:           title,
			Message:         message,
			UpdateMessageID: cardMsgID,
		},
	})
	if err != nil {
		h.logger.Error("agnes: notice send failed",
			"chat_id", chatID,
			"prompt_id", promptID,
			"error", err)
	} else {
		h.logger.Info("agnes: notice sent",
			"chat_id", chatID,
			"prompt_id", promptID)
	}
	return err
}

func (h *Handler) notifyProgress(ctx context.Context, chatID, promptID, description string) error {
	if chatID == "" {
		return fmt.Errorf("notifyProgress: chatID is empty")
	}
	h.logger.Debug("agnes: sending progress",
		"chat_id", chatID,
		"prompt_id", promptID,
		"description", truncateString(description, 50))
	err := h.rpc.SendControl(ctx, &protocol.Control{
		Type:     protocol.TypeProgress,
		PromptID: promptID,
		ChatID:   chatID,
		Progress: &protocol.ProgressPayload{Description: description},
	})
	if err != nil {
		h.logger.Warn("agnes: progress send failed",
			"chat_id", chatID,
			"prompt_id", promptID,
			"error", err)
	}
	return err
}

// splitCommand splits a prompt into "/cmd" + "rest", trimming the @-mention
// (the frontend already strips it, but this is defensive). Returns ("", prompt)
// for a non-command so /help handles bare text.
func splitCommand(prompt string) (cmd, arg string) {
	prompt = strings.TrimSpace(prompt)
	if !strings.HasPrefix(prompt, "/") {
		return "", prompt
	}
	parts := strings.SplitN(prompt, " ", 2)
	cmd = parts[0]
	if len(parts) > 1 {
		arg = strings.TrimSpace(parts[1])
	}
	return cmd, arg
}

const helpText = "**Agnes AI 后端指令**\n\n" +
	"**生成提示词**\n" +
	"• /image-prompt <图片描述> — 生成图片提示词（文生图风格）\n" +
	"• /video-prompt <视频描述> — 生成视频提示词（文生视频风格）\n\n" +
	"**生成图片/视频**\n" +
	"• /image <提示词> — 用提示词生成图片并发到群里\n" +
	"• /video <提示词> — 用提示词生成视频，返回视频链接\n\n" +
	"> 提示词可以是任意自然语言描述；也可先用 /image-prompt / /video-prompt 生成结构化提示词，再用其结果调用 /image / /video。"
