package handlers

import (
	"fmt"
	"io"
	"log"

	"github.com/mpdroog/radiusd/model"
	"github.com/mpdroog/radiusd/queue"
	"github.com/mpdroog/radiusd/radius"
	"github.com/streadway/amqp"
)

func failOnError(err error, msg string) {
	if err != nil {
		log.Println("%s: %s", msg, err)
	}
}

func btkLogger(log string) {
	conn, err := amqp.Dial("amqp://guest:guest@localhost:5672/")
	failOnError(err, "Failed to connect to RabbitMQ")
	defer conn.Close()

	ch, err := conn.Channel()
	failOnError(err, "Failed to open a channel")
	defer ch.Close()

	q, err := ch.QueueDeclare(
		"log", // name
		false, // durable
		false, // delete when unused
		false, // exclusive
		false, // no-wait
		nil,   // arguments
	)
	failOnError(err, "Failed to declare a queue")

	body := log
	err = ch.Publish(
		"",     // exchange
		q.Name, // routing key
		false,  // mandatory
		false,  // immediate
		amqp.Publishing{
			ContentType: "text/plain",
			Body:        []byte(body),
		})
	//log.Printf(" [x] Sent %s", body)
	failOnError(err, "Failed to publish a message")
}

func createSess(req *radius.Packet) model.Session {
	return model.Session{
		BytesIn:     radius.DecodeFour(req.Attr(radius.AcctInputOctets)),
		BytesOut:    radius.DecodeFour(req.Attr(radius.AcctOutputOctets)),
		PacketsIn:   radius.DecodeFour(req.Attr(radius.AcctInputPackets)),
		PacketsOut:  radius.DecodeFour(req.Attr(radius.AcctOutputPackets)),
		SessionID:   string(req.Attr(radius.AcctSessionId)),
		SessionTime: radius.DecodeFour(req.Attr(radius.AcctSessionTime)),
		User:        string(req.Attr(radius.UserName)),
		NasIP:       radius.DecodeIP(req.Attr(radius.NASIPAddress)).String(),
	}
}

func (h *Handler) AcctBegin(w io.Writer, req *radius.Packet) {
	if e := radius.ValidateAcctRequest(req); e != "" {
		h.Logger.Printf("WARN: acct.begin err=" + e)
		return
	}
	if !req.HasAttr(radius.FramedIPAddress) {
		h.Logger.Printf("WARN: acct.begin missing FramedIPAddress")
		return
	}

	user := string(req.Attr(radius.UserName))
	sess := string(req.Attr(radius.AcctSessionId))
	nasIp := radius.DecodeIP(req.Attr(radius.NASIPAddress)).String()
	clientIp := string(req.Attr(radius.CallingStationId))
	assignedIp := radius.DecodeIP(req.Attr(radius.FramedIPAddress)).String()

	if h.Verbose {
		h.Logger.Printf("acct.begin sess=%s for user=%s on nasIP=%s", sess, user, nasIp)
	}
	reply := []radius.AttrEncoder{}
	_, e := model.Limits(h.Storage, user)
	if e != nil {
		if e == model.ErrNoRows {
			h.Logger.Printf("acct.begin received invalid user=" + user)
			return
		}
		h.Logger.Printf("acct.begin e=" + e.Error())
		return
	}

	if e := model.SessionAdd(h.Storage, sess, user, nasIp, assignedIp, clientIp); e != nil {
		h.Logger.Printf("acct.begin e=%s", e.Error())
		return
	}
	w.Write(req.Response(radius.AccountingResponse, reply, h.Verbose, h.Logger))

	btkLogger(fmt.Sprintf("acct.begin sess=%s for user=%s on nasIP=%s", sess, user, nasIp))
}

func (h *Handler) AcctUpdate(w io.Writer, req *radius.Packet) {
	if e := radius.ValidateAcctRequest(req); e != "" {
		h.Logger.Printf("acct.update e=" + e)
		return
	}

	sess := createSess(req)
	if h.Verbose {
		h.Logger.Printf(
			"acct.update sess=%s for user=%s on NasIP=%s sessTime=%d octetsIn=%d octetsOut=%d",
			sess.SessionID, sess.User, sess.NasIP, sess.SessionTime, sess.BytesIn, sess.BytesOut,
		)
	}

	if e := model.SessionUpdate(h.Storage, sess); e != nil {
		h.Logger.Printf("acct.update e=" + e.Error())
		return
	}
	queue.Queue(sess.User, sess.BytesIn, sess.BytesOut, sess.PacketsIn, sess.PacketsOut)

	w.Write(radius.DefaultPacket(req, radius.AccountingResponse, "Updated accounting.", h.Verbose, h.Logger))
	btkLogger(fmt.Sprintf("acct.update sess=%s for user=%s on NasIP=%s sessTime=%d octetsIn=%d octetsOut=%d",
		sess.SessionID, sess.User, sess.NasIP, sess.SessionTime, sess.BytesIn, sess.BytesOut))
}

func (h *Handler) AcctStop(w io.Writer, req *radius.Packet) {
	if e := radius.ValidateAcctRequest(req); e != "" {
		h.Logger.Printf("acct.stop e=" + e)
		return
	}
	user := string(req.Attr(radius.UserName))
	sess := string(req.Attr(radius.AcctSessionId))
	nasIp := radius.DecodeIP(req.Attr(radius.NASIPAddress)).String()

	sessTime := radius.DecodeFour(req.Attr(radius.AcctSessionTime))
	octIn := radius.DecodeFour(req.Attr(radius.AcctInputOctets))
	octOut := radius.DecodeFour(req.Attr(radius.AcctOutputOctets))

	packIn := radius.DecodeFour(req.Attr(radius.AcctInputPackets))
	packOut := radius.DecodeFour(req.Attr(radius.AcctOutputPackets))

	termCause := radius.DecodeFour(req.Attr(radius.AcctTerminateCause))

	if h.Verbose {
		h.Logger.Printf(
			"acct.stop sess=%s for user=%s sessTime=%d octetsIn=%d octetsOut=%d",
			sess, user, sessTime, octIn, octOut,
		)
	}

	sessModel := createSess(req)
	if e := model.SessionUpdate(h.Storage, sessModel); e != nil {
		h.Logger.Printf("acct.update e=" + e.Error())
		return
	}
	if e := model.SessionLog(h.Storage, sess, user, nasIp); e != nil {
		h.Logger.Printf("acct.update e=" + e.Error())
		return
	}
	if e := model.SessionRemove(h.Storage, sess, user, nasIp); e != nil {
		h.Logger.Printf("acct.update e=" + e.Error())
		return
	}
	queue.Queue(user, octIn, octOut, packIn, packOut)

	w.Write(radius.DefaultPacket(req, radius.AccountingResponse, "Finished accounting.", h.Verbose, h.Logger))
	btkLogger(fmt.Sprintf("acct.stop sess=%s for user=%s sessTime=%d octetsIn=%d octetsOut=%d termCause=%d",
		sess, user, sessTime, octIn, octOut, termCause))
}
