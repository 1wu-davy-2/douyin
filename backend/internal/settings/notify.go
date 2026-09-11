package settings

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

const smtpTimeout = 20 * time.Second

// SendTestMail delivers a test notification using the configured SMTP
// settings. STARTTLS is used when the server advertises it; plain SMTP
// otherwise (authentication is skipped unless credentials are configured).
// Every failure is returned as an error so the API can answer
// {ok:false, error:"..."}.
func SendTestMail(cfg SMTP) error {
	if strings.TrimSpace(cfg.Host) == "" {
		return errors.New("smtp host is not configured")
	}
	if cfg.From == "" {
		return errors.New("smtp from address is not configured")
	}
	if cfg.To == "" {
		return errors.New("smtp recipient (to) is not configured")
	}
	if cfg.Port <= 0 {
		cfg.Port = 587
	}

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	conn, err := net.DialTimeout("tcp", addr, smtpTimeout)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(smtpTimeout))
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer client.Close()

	if err := client.Hello("localhost"); err != nil {
		return fmt.Errorf("ehlo: %w", err)
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if cfg.Username != "" || cfg.Password != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := client.Mail(cfg.From); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := client.Rcpt(cfg.To); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := writer.Write(buildMessage(cfg)); err != nil {
		writer.Close()
		return fmt.Errorf("write message: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish message: %w", err)
	}
	if err := client.Quit(); err != nil {
		// The message was submitted; a failed QUIT is not worth failing over.
		_ = err
	}
	return nil
}

// buildMessage renders a minimal RFC 5322 message with UTF-8 subject.
func buildMessage(cfg SMTP) []byte {
	subject := base64.StdEncoding.EncodeToString([]byte("抖音归档工具 测试邮件"))
	var b strings.Builder
	b.WriteString("From: " + cfg.From + "\r\n")
	b.WriteString("To: " + cfg.To + "\r\n")
	b.WriteString("Subject: =?utf-8?B?" + subject + "?=\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString("Douyin Archive 邮件通知配置正常。\r\n")
	return []byte(b.String())
}
