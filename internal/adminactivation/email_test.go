package adminactivation

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSMTPOptions struct {
	advertiseSTARTTLS bool
	failSTARTTLS      bool
	rejectAuth        bool
	rejectRecipient   bool
	silent            bool
}

type fakeSMTPServer struct {
	listener net.Listener
	opts     fakeSMTPOptions
	done     chan struct{}
	mu       sync.Mutex
	conns    []net.Conn
}

func startFakeSMTP(t *testing.T, opts fakeSMTPOptions) (*fakeSMTPServer, string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake SMTP: %v", err)
	}
	srv := &fakeSMTPServer{listener: listener, opts: opts, done: make(chan struct{})}
	go srv.accept()
	t.Cleanup(srv.close)

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split fake SMTP address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse fake SMTP port: %v", err)
	}
	return srv, host, port
}

func (s *fakeSMTPServer) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				continue
			}
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		go s.serve(conn)
	}
}

func (s *fakeSMTPServer) close() {
	close(s.done)
	_ = s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, conn := range s.conns {
		_ = conn.Close()
	}
}

func (s *fakeSMTPServer) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	send := func(line string) bool {
		if _, err := writer.WriteString(line + "\r\n"); err != nil {
			return false
		}
		return writer.Flush() == nil
	}

	if s.opts.silent {
		<-s.done
		return
	}
	if !send("220 fake ESMTP") {
		return
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			if !send("250-fake") {
				return
			}
			if s.opts.advertiseSTARTTLS && !send("250-STARTTLS") {
				return
			}
			// AUTH PLAIN is advertised so the sender can exercise its auth path.
			if !send("250 AUTH PLAIN") {
				return
			}
		case command == "STARTTLS":
			if s.opts.failSTARTTLS {
				_ = send("454 TLS not available")
				return
			}
			_ = send("220 Ready")
			return
		case strings.HasPrefix(command, "AUTH"):
			if s.opts.rejectAuth {
				_ = send("535 5.7.8 Authentication credentials invalid")
			} else {
				_ = send("235 2.7.0 Authentication successful")
			}
		case strings.HasPrefix(command, "MAIL FROM"):
			_ = send("250 OK")
		case strings.HasPrefix(command, "RCPT TO"):
			if s.opts.rejectRecipient {
				_ = send("550 5.1.1 Recipient rejected")
			} else {
				_ = send("250 OK")
			}
		case command == "DATA":
			if !send("354 End data with <CR><LF>.<CR><LF>") {
				return
			}
			for {
				dataLine, readErr := reader.ReadString('\n')
				if readErr != nil {
					return
				}
				if strings.TrimSpace(dataLine) == "." {
					break
				}
			}
			_ = send("250 OK queued")
		case command == "QUIT":
			_ = send("221 Bye")
			return
		case command == "NOOP", command == "RSET":
			_ = send("250 OK")
		default:
			_ = send("500 Unrecognized command")
		}
	}
}

func smtpErrorCategory(t *testing.T, err error) SMTPFailureCategory {
	t.Helper()
	var smtpErr *SMTPError
	if !errors.As(err, &smtpErr) {
		t.Fatalf("error %v is not an SMTPError", err)
	}
	return smtpErr.Category
}

func TestSMTPSenderConfigurationFailsClosedWithoutSecrets(t *testing.T) {
	sender, err := NewSMTPSender("", 587, "user", "password", "from@example.test", "starttls", time.Second)
	if sender != nil || err == nil {
		t.Fatal("expected configuration failure")
	}
	if got := smtpErrorCategory(t, err); got != SMTPFailureInvalidConfig {
		t.Fatalf("category = %q, want %q", got, SMTPFailureInvalidConfig)
	}
	if !errors.Is(err, ErrUnconfigured) {
		t.Fatal("configuration error must still satisfy ErrUnconfigured")
	}
	for _, secret := range []string{"password", "user"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("configuration error leaked a value: %q", secret)
		}
	}

	sender, err = NewSMTPSender("smtp.example.test", 587, "user", "", "from@example.test", "starttls", time.Second)
	if sender != nil || smtpErrorCategory(t, err) != SMTPFailureInvalidConfig {
		t.Fatal("partial authentication configuration must fail closed")
	}
}

func TestSMTPSenderCategorizesFailuresAndDeliversSuccessfully(t *testing.T) {
	const (
		fakeCode     = "123456"
		fakePassword = "never-log-this-password"
		recipient    = "admin@example.test"
	)

	t.Run("success", func(t *testing.T) {
		_, host, port := startFakeSMTP(t, fakeSMTPOptions{})
		sender, err := NewSMTPSender(host, port, "", "", "from@example.test", "none", time.Second)
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		if err := sender.SendActivationCode(context.Background(), recipient, fakeCode); err != nil {
			t.Fatalf("SendActivationCode: %v", err)
		}
	})

	t.Run("authentication_rejected", func(t *testing.T) {
		_, host, port := startFakeSMTP(t, fakeSMTPOptions{rejectAuth: true})
		sender, err := NewSMTPSender(host, port, "user@example.test", fakePassword, "from@example.test", "none", time.Second)
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		err = sender.SendActivationCode(context.Background(), recipient, fakeCode)
		if got := smtpErrorCategory(t, err); got != SMTPFailureAuth {
			t.Fatalf("category = %q, want %q", got, SMTPFailureAuth)
		}
	})

	t.Run("tls_negotiation_rejected", func(t *testing.T) {
		_, host, port := startFakeSMTP(t, fakeSMTPOptions{advertiseSTARTTLS: true, failSTARTTLS: true})
		sender, err := NewSMTPSender(host, port, "", "", "from@example.test", "starttls", time.Second)
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		err = sender.SendActivationCode(context.Background(), recipient, fakeCode)
		if got := smtpErrorCategory(t, err); got != SMTPFailureTLS {
			t.Fatalf("category = %q, want %q", got, SMTPFailureTLS)
		}
	})

	t.Run("starttls_missing", func(t *testing.T) {
		_, host, port := startFakeSMTP(t, fakeSMTPOptions{})
		sender, err := NewSMTPSender(host, port, "", "", "from@example.test", "starttls", time.Second)
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		err = sender.SendActivationCode(context.Background(), recipient, fakeCode)
		if got := smtpErrorCategory(t, err); got != SMTPFailureTLS {
			t.Fatalf("category = %q, want %q", got, SMTPFailureTLS)
		}
	})

	t.Run("connection_timeout", func(t *testing.T) {
		_, host, port := startFakeSMTP(t, fakeSMTPOptions{silent: true})
		sender, err := NewSMTPSender(host, port, "", "", "from@example.test", "none", 150*time.Millisecond)
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		err = sender.SendActivationCode(context.Background(), recipient, fakeCode)
		if got := smtpErrorCategory(t, err); got != SMTPFailureTimeout {
			t.Fatalf("category = %q, want %q (err=%v)", got, SMTPFailureTimeout, err)
		}
	})

	t.Run("delivery_rejected", func(t *testing.T) {
		_, host, port := startFakeSMTP(t, fakeSMTPOptions{rejectRecipient: true})
		sender, err := NewSMTPSender(host, port, "", "", "from@example.test", "none", time.Second)
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		err = sender.SendActivationCode(context.Background(), recipient, fakeCode)
		if got := smtpErrorCategory(t, err); got != SMTPFailureDelivery {
			t.Fatalf("category = %q, want %q", got, SMTPFailureDelivery)
		}
	})

	t.Run("invalid_input", func(t *testing.T) {
		_, host, port := startFakeSMTP(t, fakeSMTPOptions{})
		sender, err := NewSMTPSender(host, port, "", "", "from@example.test", "none", time.Second)
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		err = sender.SendActivationCode(context.Background(), recipient, "not-a-code")
		if got := smtpErrorCategory(t, err); got != SMTPFailureInvalidInput {
			t.Fatalf("category = %q, want %q", got, SMTPFailureInvalidInput)
		}
	})
}

func TestSMTPDiagnosticsNeverLogSecrets(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	_, host, port := startFakeSMTP(t, fakeSMTPOptions{rejectAuth: true})
	sender, err := NewSMTPSender(host, port, "user@example.test", "never-log-this-password", "from@example.test", "none", time.Second)
	if err != nil {
		t.Fatalf("NewSMTPSender: %v", err)
	}
	sender.WithLogger(logger)

	err = sender.SendActivationCode(context.Background(), "admin@example.test", "123456")
	if smtpErrorCategory(t, err) != SMTPFailureAuth {
		t.Fatalf("expected authentication category, got %v", err)
	}
	output := logs.String()
	if !strings.Contains(output, "authentication_rejected") {
		t.Fatalf("missing categorized diagnostic: %s", output)
	}
	for _, secret := range []string{"never-log-this-password", "123456", "535 5.7.8"} {
		if strings.Contains(output, secret) {
			t.Fatalf("diagnostic leaked %q: %s", secret, output)
		}
	}
	if !strings.Contains(output, "\"smtp_code\":535") {
		t.Fatalf("expected safe numeric SMTP code in diagnostic: %s", output)
	}
}
