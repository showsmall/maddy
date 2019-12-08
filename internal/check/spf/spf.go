package spf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime/debug"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-message/textproto"
	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dmarc"
	"github.com/foxcpp/maddy/internal/address"
	"github.com/foxcpp/maddy/internal/buffer"
	"github.com/foxcpp/maddy/internal/check"
	"github.com/foxcpp/maddy/internal/config"
	maddydmarc "github.com/foxcpp/maddy/internal/dmarc"
	"github.com/foxcpp/maddy/internal/dns"
	"github.com/foxcpp/maddy/internal/exterrors"
	"github.com/foxcpp/maddy/internal/log"
	"github.com/foxcpp/maddy/internal/module"
	"github.com/foxcpp/maddy/internal/target"
	"golang.org/x/net/idna"
)

const modName = "apply_spf"

type Check struct {
	instName     string
	enforceEarly bool

	noneAction     check.FailAction
	neutralAction  check.FailAction
	failAction     check.FailAction
	softfailAction check.FailAction
	permerrAction  check.FailAction
	temperrAction  check.FailAction

	log log.Logger
}

func New(_, instName string, _, _ []string) (module.Module, error) {
	return &Check{
		instName: instName,
		log:      log.Logger{Name: modName},
	}, nil
}

func (c *Check) Name() string {
	return modName
}

func (c *Check) InstanceName() string {
	return c.instName
}

func (c *Check) Init(cfg *config.Map) error {
	cfg.Bool("debug", true, false, &c.log.Debug)
	cfg.Bool("enforce_early", true, false, &c.enforceEarly)
	cfg.Custom("none_action", false, false,
		func() (interface{}, error) {
			return check.FailAction{}, nil
		}, check.FailActionDirective, &c.noneAction)
	cfg.Custom("neutral_action", false, false,
		func() (interface{}, error) {
			return check.FailAction{}, nil
		}, check.FailActionDirective, &c.neutralAction)
	cfg.Custom("fail_action", false, false,
		func() (interface{}, error) {
			return check.FailAction{Quarantine: true}, nil
		}, check.FailActionDirective, &c.failAction)
	cfg.Custom("softfail_action", false, false,
		func() (interface{}, error) {
			return check.FailAction{Quarantine: true}, nil
		}, check.FailActionDirective, &c.softfailAction)
	cfg.Custom("permerr_action", false, false,
		func() (interface{}, error) {
			return check.FailAction{Reject: true}, nil
		}, check.FailActionDirective, &c.permerrAction)
	cfg.Custom("temperr_action", false, false,
		func() (interface{}, error) {
			return check.FailAction{Reject: true}, nil
		}, check.FailActionDirective, &c.temperrAction)
	_, err := cfg.Process()
	if err != nil {
		return err
	}

	return nil
}

type spfRes struct {
	res spf.Result
	err error
}

type state struct {
	c        *Check
	msgMeta  *module.MsgMetadata
	spfFetch chan spfRes
	log      log.Logger
}

func (c *Check) CheckStateForMsg(msgMeta *module.MsgMetadata) (module.CheckState, error) {
	return &state{
		c:        c,
		msgMeta:  msgMeta,
		spfFetch: make(chan spfRes, 1),
		log:      target.DeliveryLogger(c.log, msgMeta),
	}, nil
}

func (s *state) spfResult(res spf.Result, err error) module.CheckResult {
	_, fromDomain, _ := address.Split(s.msgMeta.OriginalFrom)
	spfAuth := &authres.SPFResult{
		Value: authres.ResultNone,
		Helo:  s.msgMeta.Conn.Hostname,
		From:  fromDomain,
	}

	if err != nil {
		spfAuth.Reason = err.Error()
	} else if res == spf.None {
		spfAuth.Reason = "no policy"
	}

	switch res {
	case spf.None:
		spfAuth.Value = authres.ResultNone
		return s.c.noneAction.Apply(module.CheckResult{
			Reason: &exterrors.SMTPError{
				Code:         550,
				EnhancedCode: exterrors.EnhancedCode{5, 7, 23},
				Message:      "No SPF policy",
				CheckName:    modName,
			},
			AuthResult: []authres.Result{spfAuth},
		})
	case spf.Neutral:
		spfAuth.Value = authres.ResultNeutral
		return s.c.neutralAction.Apply(module.CheckResult{
			Reason: &exterrors.SMTPError{
				Code:         550,
				EnhancedCode: exterrors.EnhancedCode{5, 7, 23},
				Message:      "Neutral SPF result is not permitted",
				CheckName:    modName,
			},
			AuthResult: []authres.Result{spfAuth},
		})
	case spf.Pass:
		spfAuth.Value = authres.ResultPass
		return module.CheckResult{AuthResult: []authres.Result{spfAuth}}
	case spf.Fail:
		spfAuth.Value = authres.ResultFail
		return s.c.failAction.Apply(module.CheckResult{
			Reason: &exterrors.SMTPError{
				Code:         550,
				EnhancedCode: exterrors.EnhancedCode{5, 7, 23},
				Message:      "SPF authentication failed",
				CheckName:    modName,
			},
			AuthResult: []authres.Result{spfAuth},
		})
	case spf.SoftFail:
		spfAuth.Value = authres.ResultSoftFail
		return s.c.softfailAction.Apply(module.CheckResult{
			Reason: &exterrors.SMTPError{
				Code:         550,
				EnhancedCode: exterrors.EnhancedCode{5, 7, 23},
				Message:      "SPF authentication soft-failed",
				CheckName:    modName,
			},
			AuthResult: []authres.Result{spfAuth},
		})
	case spf.TempError:
		spfAuth.Value = authres.ResultTempError
		return s.c.temperrAction.Apply(module.CheckResult{
			Reason: &exterrors.SMTPError{
				Code:         451,
				EnhancedCode: exterrors.EnhancedCode{4, 7, 23},
				Message:      "SPF authentication failed with temporary error",
				CheckName:    modName,
			},
			AuthResult: []authres.Result{spfAuth},
		})
	case spf.PermError:
		spfAuth.Value = authres.ResultPermError
		return s.c.permerrAction.Apply(module.CheckResult{
			Reason: &exterrors.SMTPError{
				Code:         550,
				EnhancedCode: exterrors.EnhancedCode{4, 7, 23},
				Message:      "SPF authentication failed with permanent error",
				CheckName:    modName,
			},
			AuthResult: []authres.Result{spfAuth},
		})
	}

	return module.CheckResult{
		Reason: &exterrors.SMTPError{
			Code:         550,
			EnhancedCode: exterrors.EnhancedCode{4, 7, 23},
			Message:      fmt.Sprintf("Unknown SPF status: %s", res),
			CheckName:    modName,
		},
		AuthResult: []authres.Result{spfAuth},
	}
}

func (s *state) relyOnDMARC(hdr textproto.Header) bool {
	fromDomain, err := maddydmarc.ExtractFromDomain(hdr)
	if err != nil {
		s.log.Error("DMARC domains extract", err)
		return false
	}

	policyDomain, record, err := maddydmarc.FetchRecord(dns.DefaultResolver(), context.Background(), fromDomain)
	if err != nil {
		s.log.Error("DMARC fetch", err, "from_domain", fromDomain)
		return false
	}
	if record == nil {
		return false
	}

	policy := record.Policy
	// TODO: Is it ok to use EqualFold for subdomain check?
	if !dns.Equal(policyDomain, fromDomain) && record.SubdomainPolicy != "" {
		policy = record.SubdomainPolicy
	}

	return policy != dmarc.PolicyNone
}

func prepareMailFrom(from string) (string, error) {
	// INTERNATIONALIZATION: RFC 8616, Section 4
	// Hostname is already in A-labels per SMTPUTF8 requirement.
	// MAIL FROM domain should be converted to A-labels before doing
	// anything.
	fromMbox, fromDomain, err := address.Split(from)
	if err != nil {
		return "", &exterrors.SMTPError{
			Code:         550,
			EnhancedCode: exterrors.EnhancedCode{5, 1, 7},
			Message:      "Malformed address",
			CheckName:    "spf",
		}
	}
	fromDomain, err = idna.ToASCII(fromDomain)
	if err != nil {
		return "", &exterrors.SMTPError{
			Code:         550,
			EnhancedCode: exterrors.EnhancedCode{5, 1, 7},
			Message:      "Malformed address",
			CheckName:    "spf",
		}
	}

	// %{s} and %{l} do not match anything if it is non-ASCII.
	// Since spf lib does not seem to care, strip it.
	if !address.IsASCII(fromMbox) {
		fromMbox = ""
	}

	return fromMbox + "@" + fromDomain, nil
}

func (s *state) CheckConnection() module.CheckResult {
	if s.msgMeta.Conn == nil {
		s.log.Println("locally generated message, skipping")
		return module.CheckResult{}
	}

	ip, ok := s.msgMeta.Conn.RemoteAddr.(*net.TCPAddr)
	if !ok {
		s.log.Println("non-IP SrcAddr")
		return module.CheckResult{}
	}

	mailFrom, err := prepareMailFrom(s.msgMeta.OriginalFrom)
	if err != nil {
		return module.CheckResult{
			Reason: err,
			Reject: true,
		}
	}

	if s.c.enforceEarly {
		res, err := spf.CheckHostWithSender(ip.IP, s.msgMeta.Conn.Hostname, mailFrom)
		s.log.Debugf("result: %s (%v)", res, err)
		return s.spfResult(res, err)
	}

	// We start evaluation in parallel to other message processing,
	// once we get the body, we fetch DMARC policy and see if it exists
	// and not p=none. In that case, we rely on DMARC alignment to define result.
	// Otherwise, we take action based on SPF only.

	go func() {
		defer func() {
			if err := recover(); err != nil {
				stack := debug.Stack()
				log.Printf("panic during spf.CheckHostWithSender: %v\n%s", err, stack)
				close(s.spfFetch)
			}
		}()

		res, err := spf.CheckHostWithSender(ip.IP, s.msgMeta.Conn.Hostname, mailFrom)
		s.log.Debugf("result: %s (%v)", res, err)
		s.spfFetch <- spfRes{res, err}
	}()

	return module.CheckResult{}
}

func (s *state) CheckSender(mailFrom string) module.CheckResult {
	return module.CheckResult{}
}

func (s *state) CheckRcpt(rcptTo string) module.CheckResult {
	return module.CheckResult{}
}

func (s *state) CheckBody(header textproto.Header, body buffer.Buffer) module.CheckResult {
	if s.c.enforceEarly {
		// Already applied in CheckConnection.
		return module.CheckResult{}
	}

	res, ok := <-s.spfFetch
	if !ok {
		return module.CheckResult{
			Reject: true,
			Reason: exterrors.WithTemporary(
				exterrors.WithFields(errors.New("panic recovered"), map[string]interface{}{
					"check":    "spf",
					"smtp_msg": "Internal error during policy check",
				}),
				true,
			),
		}
	}
	if s.relyOnDMARC(header) {
		if res.res != spf.Pass || res.err != nil {
			s.log.Printf("deferring action due to a DMARC policy")
		} else {
			s.log.Debugf("deferring action due to a DMARC policy")
		}

		checkRes := s.spfResult(res.res, res.err)
		checkRes.Quarantine = false
		checkRes.Reject = false
		return checkRes
	}

	return s.spfResult(res.res, res.err)
}

func (s *state) Close() error {
	return nil
}

func init() {
	module.Register(modName, New)
}