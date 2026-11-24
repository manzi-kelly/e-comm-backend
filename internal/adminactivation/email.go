package adminactivation

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// SMTPFailureCategory is a safe, non-sensitive classification of a delivery
// failure. Categories never include server replies, credentials, codes or tokens.
type SMTPFailureCategory string

const (
	SMTPFailureInvalidConfig SMTPFailureCategory = "invalid_sender_configuration"
	SMTPFailureInvalidInput  SMTPFailureCategory = "invalid_input"
	SMTPFailureConnection    SMTPFailureCategory = "connection_failed"
	SMTPFailureTimeout       SMTPFailureCategory = "connection_timeout"
	SMTPFailureTLS           SMTPFailureCategory = "tls_negotiation_failed"
	SMTPFailureAuth          SMTPFailureCategory = "authentication_rejected"
	SMTPFailureDelivery      SMTPFailureCategory = "mail_delivery_rejected"
)

// SMTPError is the only error shape emitted by SMTPSender. Its Error() text is
// deliberately static: raw SMTP responses are never propagated or logged.
type SMTPError struct {
	Category  SMTPFailureCategory
	Operation string
}

func (e *SMTPError) Error() string {
	if e == nil {
		return ""
	}
	return "activation smtp " + string(e.Category) + " during " + e.Operation
}

func (e *SMTPError) Unwrap() error {
	if e != nil && e.Category == SMTPFailureInvalidConfig {
		return ErrUnconfigured
	}
	return nil
}

// SMTPSender delivers activation codes through an SMTP relay. It never logs
// message contents, codes, credentials, or sensitive SMTP responses.
type SMTPSender struct {
	host     string
	port     int
	username string
	password string
	from     string
	tlsMode  string
	timeout  time.Duration
	logger   *slog.Logger
}

// NewSMTPSender fails closed unless the activation configuration is complete.
func NewSMTPSender(host string, port int, username, password, from, tlsMode string, timeout time.Duration) (*SMTPSender, error) {
	host = strings.TrimSpace(host)
	from = strings.TrimSpace(from)
	username = strings.TrimSpace(username)
	if host == "" || from == "" || port < 1 || port > 65535 || timeout <= 0 {
		return nil, smtpFailure(SMTPFailureInvalidConfig, "configuration")
	}
	if tlsMode != "starttls" && tlsMode != "tls" && tlsMode != "none" {
		return nil, smtpFailure(SMTPFailureInvalidConfig, "configuration")
	}
	if (username == "") != (password == "") {
		return nil, smtpFailure(SMTPFailureInvalidConfig, "configuration")
	}
	return &SMTPSender{
		host: host, port: port, username: username,
		password: password, from: from, tlsMode: tlsMode, timeout: timeout,
	}, nil
}

// WithLogger attaches a sanitized diagnostic logger. It is safe to call once
// at startup; the returned sender is the same instance for easy chaining.
func (s *SMTPSender) WithLogger(logger *slog.Logger) *SMTPSender {
	if s != nil && logger != nil {
		s.logger = logger
	}
	return s
}

func smtpFailure(category SMTPFailureCategory, operation string) error {
	return &SMTPError{Category: category, Operation: operation}
}

// fail records only a category, static operation label, host/port and (when
// present) the numeric SMTP status code. Raw error text is discarded.
func (s *SMTPSender) fail(category SMTPFailureCategory, operation string, cause error) error {
	err := smtpFailure(category, operation)
	if s == nil || s.logger == nil {
		return err
	}
	attrs := []any{
		"category", string(category),
		"operation", operation,
		"host", s.host,
		"port", s.port,
	}
	if code := smtpStatusCode(cause); code > 0 {
		attrs = append(attrs, "smtp_code", code)
	}
	if isTimeout(cause) {
		attrs = append(attrs, "timeout_ms", s.timeout.Milliseconds())
	}
	s.logger.Warn("activation smtp delivery failed", attrs...)
	return err
}

func (s *SMTPSender) failWithFallback(category SMTPFailureCategory, operation string, cause error) error {
	if isTimeout(cause) {
		return s.fail(SMTPFailureTimeout, operation, cause)
	}
	return s.fail(category, operation, cause)
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func smtpStatusCode(err error) int {
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		return protoErr.Code
	}
	return 0
}

func (s *SMTPSender) SendActivationCode(ctx context.Context, to, code string) error {
	if err := ctx.Err(); err != nil {
		return s.failWithFallback(SMTPFailureConnection, "context", err)
	}
	if !ValidCodeFormat(code) || strings.ContainsAny(to, "\r\n") {
		return s.fail(SMTPFailureInvalidInput, "input", nil)
	}

	deadline := time.Now().Add(s.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := &net.Dialer{Timeout: s.timeout, Deadline: deadline}
	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	tlsConfig := &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	var client *smtp.Client
	var err error
	if s.tlsMode == "tls" {
		tlsConn, dialErr := tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
		if dialErr != nil {
			return s.failWithFallback(SMTPFailureTLS, "tls_dial", dialErr)
		}
		conn = tlsConn
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return s.failWithFallback(SMTPFailureConnection, "connect", err)
		}
	}
	_ = conn.SetDeadline(deadline)

	client, err = smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return s.failWithFallback(SMTPFailureConnection, "greeting", err)
	}
	defer func() { _ = client.Close() }()

	if s.tlsMode == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return s.fail(SMTPFailureTLS, "starttls_extension", nil)
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return s.failWithFallback(SMTPFailureTLS, "starttls", err)
		}
	}
	if s.username != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			return s.fail(SMTPFailureAuth, "auth_extension", nil)
		}
		if err := client.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
			return s.failWithFallback(SMTPFailureAuth, "authentication", err)
		}
	}
	if err := client.Mail(s.from); err != nil {
		return s.failWithFallback(SMTPFailureDelivery, "envelope_sender", err)
	}
	if err := client.Rcpt(to); err != nil {
		return s.failWithFallback(SMTPFailureDelivery, "recipient", err)
	}
	writer, err := client.Data()
	if err != nil {
		return s.failWithFallback(SMTPFailureDelivery, "data_channel", err)
	}
	message := "From: AutivaShop <" + s.from + ">\r\n" +
		"To: " + to + "\r\n" +
		"Subject: AutivaShop administrator activation code\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"\r\n" +
		"Your AutivaShop administrator activation code is:\r\n\r\n" +
		code + "\r\n\r\n" +
		"It expires in five minutes. If you did not request it, ignore this message.\r\n"
	if _, err := io.WriteString(writer, message); err != nil {
		_ = writer.Close()
		return s.failWithFallback(SMTPFailureDelivery, "message_write", err)
	}
	if err := writer.Close(); err != nil {
		return s.failWithFallback(SMTPFailureDelivery, "message_rejected", err)
	}
	_ = client.Quit()
	return nil
}
