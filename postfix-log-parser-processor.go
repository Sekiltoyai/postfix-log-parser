package postfixlog

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

type (
	PostfixLogProcessor struct {
		MqMtx       sync.Mutex
		Mqueue      map[string]*PostfixLogParser
		PostfixLog  *PostfixLog
		Outputcb    func(value interface{}) error
		Statscb     func(statname string, hostname string, inc bool, value float64)
		Flatten		bool
	}

	Message struct {
		Time     *time.Time `json:"time"`
		To       string     `json:"to"`
		Status   string     `json:"status"`
		Message  string     `json:"message"`
		BounceId string     `json:"bounce_id"`
	}

	PostfixLogParser struct {
		Time           *time.Time `json:"time"`
		Hostname       string     `json:"hostname"`
		Process        string     `json:"process"`
		QueueId        string     `json:"queue_id"`
		ClientHostname string     `json:"client_hostname"`
		ClinetIp       string     `json:"client_ip"`
		SaslMethod     string     `json:"sasl_method"`
		SaslUsername   string     `json:"sasl_username"`
		MessageId      string     `json:"message_id"`
		From           string     `json:"from"`
		Size           string     `json:"size"`
		NRcpt          string     `json:"nrcpt"`
		Messages       []Message  `json:"messages"`
	}

	PostfixLogParserFlat struct {
		Time           *time.Time `json:"time"`
		Hostname       string     `json:"hostname"`
		Process        string     `json:"process"`
		QueueId        string     `json:"queue_id"`
		ClientHostname string     `json:"client_hostname"`
		ClinetIp       string     `json:"client_ip"`
		SaslMethod     string     `json:"sasl_method"`
		SaslUsername   string     `json:"sasl_username"`
		MessageId      string     `json:"message_id"`
		From           string     `json:"from"`
		Size           string     `json:"size"`
		NRcpt          string     `json:"nrcpt"`
		TimeSent       *time.Time `json:"time_sent"`
		To             string     `json:"to"`
		Status         string     `json:"status"`
		Message        string     `json:"message"`
		BounceId       string     `json:"bounce_id"`
	}
)

func NewPostfixLogProcessor(outputcb func(value interface{}) error,
	statscb func(statname string, hostname string, inc bool, value float64),
	flatten bool) *PostfixLogProcessor {

	var mqueue = make(map[string]*PostfixLogParser)
	var p = NewPostfixLog()

	return &PostfixLogProcessor{
		// create map of messages
		Mqueue: mqueue,
		// Initialize postfix log parser
		PostfixLog: p,
		Outputcb: outputcb,
		Statscb: statscb,
		Flatten: flatten,
	}
}

func (pp *PostfixLogProcessor) PlpToFlat(plp *PostfixLogParser) []PostfixLogParserFlat {
	var plpf = make([]PostfixLogParserFlat, len(plp.Messages))

	for i := range plp.Messages {
		plpf[i] = PostfixLogParserFlat{
			Time:           plp.Time,
			Hostname:       plp.Hostname,
			Process:        plp.Process,
			QueueId:        plp.QueueId,
			ClientHostname: plp.ClientHostname,
			ClinetIp:       plp.ClinetIp,
			SaslMethod:     plp.SaslMethod,
			SaslUsername:   plp.SaslUsername,
			MessageId:      plp.MessageId,
			From:           plp.From,
			Size:           plp.Size,
			NRcpt:          plp.NRcpt,
			TimeSent:       plp.Messages[i].Time,
			To:             plp.Messages[i].To,
			Status:         plp.Messages[i].Status,
			Message:        plp.Messages[i].Message,
			BounceId:       plp.Messages[i].BounceId,
		}
	}

	return plpf
}

// Remove sent, milter-rejected and deferred that entered queue more than "duration" ago
func (pp *PostfixLogProcessor) CleanMQueue(age time.Duration) {
	var ok int

	log.Printf("Start cleaning queue task: %d items in queue", len(pp.Mqueue))

	// We need read lock: fatal error: concurrent map iteration and map write
	pp.MqMtx.Lock()
	for qid, inmail := range pp.Mqueue {
		ok = 0
		// Check all mails were sent (multiple destinations mails)
		for _, outmail := range inmail.Messages {
			// Sent and Rejected mails won't have any other event, we can rm
			if outmail.Status == "sent" || outmail.Status == "milter-reject" || outmail.Status == "postfix-reject" {
				ok += 1
			} else if outmail.Status == "deferred" {
				if inmail.Time.Add(age).Before(time.Now()) {
					ok += 1
				}
			}
		}

		if ok == len(inmail.Messages) {
			// We already in rw lock
			delete(pp.Mqueue, qid)
		}
	}
	pp.MqMtx.Unlock()
	log.Printf("Finished cleaning queue task: %d items in queue", len(pp.Mqueue))
}

/*
 * This is the function doing the work.
 * Each input is stored in a map,
 * then written to output when we recognize it as the last line
 */
func (pp *PostfixLogProcessor) ParseStoreAndWrite(input []byte) error {
	logFormat, err := pp.PostfixLog.Parse(input)
	if err != nil {
		// Incorrect line, just skip it
		if err.Error() == "Error: Line do not match regex" {
			pp.Statscb("lineincorrectcnt", "", true, 0)
			return err
		}
		return err
	}

	/*
		Oct 10 04:02:02 mail.example.com postfix/smtpd[22941]: DFBEFDBF00C5: client=example.net[127.0.0.1], sasl_method=PLAIN, sasl_username=user@example.com
	*/
	if logFormat.ClientHostname != "" && !strings.HasPrefix(logFormat.Messages, "milter-reject:") {
		pp.MqMtx.Lock()
		pp.Mqueue[logFormat.QueueId] = &PostfixLogParser{
			Time:           logFormat.Time,
			Hostname:       logFormat.Hostname,
			Process:        logFormat.Process,
			QueueId:        logFormat.QueueId,
			ClientHostname: logFormat.ClientHostname,
			ClinetIp:       logFormat.ClinetIp,
			SaslMethod:     logFormat.SaslMethod,
			SaslUsername:   logFormat.SaslUsername,
		}
		pp.MqMtx.Unlock()
	}

	/*
		Oct 10 04:02:02 mail.example.com postfix/cleanup[22923]: DFBEFDBF00C5: message-id=<20181009190202.81363306015D@example.com>
	*/
	if logFormat.MessageId != "" {
		pp.MqMtx.Lock()
		if plp, ok := pp.Mqueue[logFormat.QueueId]; ok {
			plp.MessageId = logFormat.MessageId
		}
		pp.MqMtx.Unlock()
	}

	/*
		Oct 10 04:02:03 mail.example.com postfix/qmgr[18719]: DFBEFDBF00C5: from=<root@example.com>, size=3578, nrcpt=1 (queue active)
	*/
	if logFormat.From != "" {
		pp.MqMtx.Lock()
		if plp, ok := pp.Mqueue[logFormat.QueueId]; ok {
			plp.From = logFormat.From
			plp.Size = logFormat.Size
			plp.NRcpt = logFormat.NRcpt
		}
		pp.MqMtx.Unlock()
		nrcpt, _ := strconv.ParseFloat(logFormat.NRcpt, 64)
		pp.Statscb("msgincnt", logFormat.Hostname, false, nrcpt)
	}

	/*
		Oct 10 04:02:08 mail.example.com postfix/smtp[22928]: DFBEFDBF00C5: to=<test@example-to.com>, relay=mail.example-to.com[192.168.0.10]:25, delay=5.3, delays=0.26/0/0.31/4.7, dsn=2.0.0, status=sent (250 2.0.0 Ok: queued as C598F1B0002D)
	*/
	if logFormat.To != "" {
		pp.MqMtx.Lock()
		if plp, ok := pp.Mqueue[logFormat.QueueId]; ok {
			message := Message{
				Time:     logFormat.Time,
				To:       logFormat.To,
				Status:   logFormat.Status,
				Message:  logFormat.Messages,
				BounceId: "",
			}
			/* When a message is deferred, it won't be written out until it is either sent, expired, or generates a non delivery notification.
			We want to know instantly when a message is deferred, so we handle this case by emiting output for this message, and not appending this occurence
			to the list of Messages
			*/
			if logFormat.Status == "deferred" {
				pp.Statscb("msgdeferredcnt", plp.Hostname, true, 0)
				tmpplp := PostfixLogParser{
					Time:           plp.Time,
					Hostname:       plp.Hostname,
					Process:        plp.Process,
					QueueId:        plp.QueueId,
					ClientHostname: plp.ClientHostname,
					ClinetIp:       plp.ClinetIp,
					SaslMethod:     plp.SaslMethod,
					SaslUsername:   plp.SaslUsername,
					MessageId:      plp.MessageId,
					From:           plp.From,
					Size:           plp.Size,
					NRcpt:          plp.NRcpt,
				}
				tmpplp.Messages = append(tmpplp.Messages, message)

				if pp.Flatten {
					err = pp.Outputcb(pp.PlpToFlat(&tmpplp)[0])
				} else {
					err = pp.Outputcb(tmpplp)
				}
				if (err != nil) {
					log.Fatal(err)
				}

				tmpplp.Messages = nil
				// cannot use nil as type PostfixLogParser in assignment
				//tmpplp = nil
			} else {
				plp.Messages = append(plp.Messages, message)
			}
		}
		pp.MqMtx.Unlock()
	}

	/*
		2021-02-05T17:25:03+01:00 mail.example.com postfix/bounce[39258]: 006B056E6: sender non-delivery notification: 642E456E9
	*/
	if logFormat.BounceId != "" {
		pp.MqMtx.Lock()
		if plp, ok := pp.Mqueue[logFormat.QueueId]; ok {
			// Get the matching Message by Status=bounced
			for i, msg := range plp.Messages {
				// Need to manage more than one bounce for the same queue_id. This is flawy as we just rely on order to match
				if msg.Status == "bounced" && len(msg.BounceId) == 0 {
					message := Message{
						Time:     msg.Time,
						To:       msg.To,
						Status:   msg.Status,
						Message:  msg.Message,
						BounceId: logFormat.BounceId,
					}
					// Delete old message, put new at the end
					copy(plp.Messages[i:], plp.Messages[i+1:])
					plp.Messages[len(plp.Messages)-1] = message
					break
				}
			}
		}
		pp.MqMtx.Unlock()
	}
	/*
		Oct 10 04:02:08 mail.example.com postfix/qmgr[18719]: DFBEFDBF00C5: removed
			or
		2021-02-05T14:17:51+01:00 smtp.server.com postfix/cleanup[38982]: D8C136A3A: milter-reject: END-OF-MESSAGE from unknown[1.2.3.4]: 4.7.1 Greylisting in action, try again later; from=<sender1@sender.com> to=<dest1@example.com> proto=ESMTP helo=<mail.sender.com>
		    or
		2023-12-22T09:11:42.627010+01:00 smtp-02.example.org postfix/smtpd[2717] 6CB5F45B78: reject: RCPT from unknown[11.12.13.14]: 450 4.1.2 <mark@distant.domain.nz>: Recipient address rejected: Domain not found; from=<a.user@a.domain.fr> to=<mark@distant.domain.nz> proto=ESMTP helo=<mail.a.domain.fr>
	*/
	// "removed" message is end of logs. then flush.
	if logFormat.Messages == "removed" || strings.HasPrefix(logFormat.Status, "milter-") || strings.EqualFold(logFormat.Status, "postfix-reject") {
		pp.MqMtx.Lock()
		if plp, ok := pp.Mqueue[logFormat.QueueId]; ok {
			for _, plpf := range pp.PlpToFlat(plp) {
				switch plpf.Status {
				case "sent":
					pp.Statscb("msgsentcnt", plpf.Hostname, true, 0)
				case "milter-reject", "postfix-reject":
					pp.Statscb("msgrejectedcnt", plpf.Hostname, true, 0)
				case "milter-hold":
					pp.Statscb("msgholdcnt", plpf.Hostname, true, 0)
				case "bounced":
					pp.Statscb("msgbouncedcnt", plpf.Hostname, true, 0)
				}

				if pp.Flatten {
					err = pp.Outputcb(plpf)
					if err != nil {
						log.Fatal(err)
					}
				}
			}

			if !pp.Flatten {
				err = pp.Outputcb(plp)
				if err != nil {
					log.Fatal(err)
				}
			}
		}
		pp.MqMtx.Unlock()
	}

	/*
		2022-06-29T10:55:18.498553+02:00 srv-smtp-01.domain.com postfix/smtpd[75994] warning: unknown[10.11.12.13]: SASL LOGIN authentication failed: authentication failure
	*/
	// An auth failed message is not queued, we just write and forget
	if strings.EqualFold(logFormat.Status, "auth-failed") {
		pp.Statscb("msgauthfailed", logFormat.Hostname, true, 0)
		message := Message{
			Time:    logFormat.Time,
			Status:  logFormat.Status,
			Message: logFormat.Messages,
		}

		tmpplp := PostfixLogParser{
			Time:           logFormat.Time,
			Hostname:       logFormat.Hostname,
			Process:        logFormat.Process,
			ClientHostname: logFormat.ClientHostname,
			ClinetIp:       logFormat.ClinetIp,
			SaslMethod:     logFormat.SaslMethod,
		}
		tmpplp.Messages = append(tmpplp.Messages, message)

		if pp.Flatten {
			err = pp.Outputcb(pp.PlpToFlat(&tmpplp)[0])
		} else {
			err = pp.Outputcb(tmpplp)
		}
		if err != nil {
			log.Fatal(err)
		}
	}

	return nil
}
