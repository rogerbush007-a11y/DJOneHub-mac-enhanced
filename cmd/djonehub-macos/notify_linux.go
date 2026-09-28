//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// desktopNotifier shows incoming calls and SMS as GNOME (freedesktop)
// notifications through notify-send, so neither needs the web console open.
//
// A ringing call gets a critical notification with answer / decline buttons
// and a looping ringtone. It is withdrawn once the call is answered anywhere
// or ends, and an unanswered incoming call leaves a missed-call notification.
// The URC events make it appear at once; the call poll's notifier is the
// fallback when a URC was lost.
type desktopNotifier struct {
	app *app
	url string

	mu        sync.Mutex
	ringing   bool
	answered  bool
	number    string
	callID    uint32
	ringStop  context.CancelFunc
	ringTimer *time.Timer
	started   time.Time
}

const (
	ringtonePath    = "/usr/share/sounds/freedesktop/stereo/phone-incoming-call.oga"
	desktopEntryID  = "djonehub"
	notifyAppName   = "DJOneHub"
	ringFallbackGap = 800 * time.Millisecond
	// smsNotifyGrace keeps a restart from re-announcing messages that were
	// already sitting on the SIM.
	smsNotifyGrace = 5 * time.Minute
)

func (a *app) startDesktopNotifier(listen string) {
	if a.events == nil {
		return
	}
	if _, err := exec.LookPath("notify-send"); err != nil {
		log.Printf("desktop notifications unavailable: %v", err)
		return
	}
	n := &desktopNotifier{app: a, url: "http://" + listen, started: time.Now()}
	a.callNotifier = func(call callRecord) { n.incoming(call.Number) }
	a.smsNotifier = n.sms
	a.callDeclined = n.declined
	stream, _ := a.events.subscribe()
	go func() {
		for event := range stream {
			n.handle(event)
		}
	}()
	log.Printf("desktop notifications enabled (notify-send)")
}

func (n *desktopNotifier) handle(event callEvent) {
	switch event.Type {
	case "ring":
		// RING precedes +CLIP; give the number a moment to arrive so the
		// notification does not have to be replaced right away.
		n.mu.Lock()
		if !n.ringing && n.ringTimer == nil {
			n.ringTimer = time.AfterFunc(ringFallbackGap, func() { n.incoming("") })
		}
		n.mu.Unlock()
	case "caller":
		n.incoming(event.Number)
	case "connected":
		n.callAnswered()
	case "state":
		switch event.State {
		case "active":
			n.callAnswered()
		case "incoming", "waiting":
			n.incoming(event.Number)
		}
	case "ended":
		n.callEnded()
	}
}

func (n *desktopNotifier) incoming(number string) {
	n.mu.Lock()
	if n.ringTimer != nil {
		n.ringTimer.Stop()
		n.ringTimer = nil
	}
	if n.ringing {
		// Already shown. A number arriving late is kept for the missed-call
		// notice; replacing the live notification would leave two
		// notify-send processes answering to the same buttons.
		if number != "" && n.number == "" {
			n.number = number
		}
		n.mu.Unlock()
		return
	}
	n.ringing = true
	n.answered = false
	n.number = number
	ctx, cancel := context.WithCancel(context.Background())
	n.ringStop = cancel
	n.mu.Unlock()

	go loopRingtone(ctx)
	go n.showCall(number)
}

func displayNumber(number string) string {
	if strings.TrimSpace(number) == "" {
		return "未知号码"
	}
	return number
}

// showCall posts the ringing notification and acts on the button pressed.
func (n *desktopNotifier) showCall(number string) {
	args := []string{
		"--app-name=" + notifyAppName,
		"--icon=call-start",
		"--urgency=critical",
		"--category=call.incoming",
		"--hint=string:desktop-entry:" + desktopEntryID,
		"--print-id",
		"--action=answer=接听",
		"--action=reject=拒接",
		"--action=default=打开 DJOneHub",
	}
	args = append(args, "来电", displayNumber(number))

	action, id, err := runNotify(args, func(id uint32) {
		n.mu.Lock()
		if n.ringing {
			n.callID = id
		}
		stillRinging := n.ringing
		n.mu.Unlock()
		if !stillRinging {
			closeNotification(id)
		}
	})
	if err != nil {
		log.Printf("call notification: %v", err)
		return
	}
	n.mu.Lock()
	current := n.callID == id
	n.mu.Unlock()
	if !current {
		return
	}
	switch action {
	case "answer":
		n.stopRinging()
		if _, err := n.app.answerIncoming(); err != nil {
			log.Printf("answer from notification: %v", err)
			notifySimple("接听失败", err.Error(), "dialog-error")
		}
	case "reject":
		n.stopRinging()
		if _, err := n.app.rejectIncoming(); err != nil {
			log.Printf("reject from notification: %v", err)
		}
	case "default":
		openURL(n.url)
	}
}

func (n *desktopNotifier) stopRinging() {
	n.mu.Lock()
	if n.ringStop != nil {
		n.ringStop()
		n.ringStop = nil
	}
	n.mu.Unlock()
}

func (n *desktopNotifier) withdraw() uint32 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ringTimer != nil {
		n.ringTimer.Stop()
		n.ringTimer = nil
	}
	if n.ringStop != nil {
		n.ringStop()
		n.ringStop = nil
	}
	id := n.callID
	n.callID = 0
	return id
}

// declined marks the ringing call as refused on purpose, from the
// notification or the web console, so it is not reported as missed.
func (n *desktopNotifier) declined() {
	n.mu.Lock()
	if n.ringing {
		n.answered = true
	}
	n.mu.Unlock()
	n.stopRinging()
}

func (n *desktopNotifier) callAnswered() {
	n.mu.Lock()
	if n.ringing {
		n.answered = true
	}
	n.mu.Unlock()
	if id := n.withdraw(); id != 0 {
		closeNotification(id)
	}
}

func (n *desktopNotifier) callEnded() {
	id := n.withdraw()
	n.mu.Lock()
	missed := n.ringing && !n.answered
	number := n.number
	n.ringing, n.answered, n.number = false, false, ""
	n.mu.Unlock()
	if id != 0 {
		closeNotification(id)
	}
	if missed {
		go func() {
			action, _, err := runNotify([]string{
				"--app-name=" + notifyAppName,
				"--icon=call-missed",
				"--category=call.unanswered",
				"--hint=string:desktop-entry:" + desktopEntryID,
				"--action=default=打开 DJOneHub",
				"未接来电",
				fmt.Sprintf("%s · %s", displayNumber(number), time.Now().Format("15:04")),
			}, nil)
			if err == nil && action == "default" {
				openURL(n.url)
			}
		}()
	}
}

func (n *desktopNotifier) sms(msg receivedSMS) {
	if !msg.Timestamp.IsZero() && msg.Timestamp.Before(n.started.Add(-smsNotifyGrace)) {
		return
	}
	title := "短信 · " + displayNumber(msg.Sender)
	args := []string{
		"--app-name=" + notifyAppName,
		"--icon=mail-message-new",
		"--category=im.received",
		"--hint=string:desktop-entry:" + desktopEntryID,
		"--hint=string:sound-name:message-new-instant",
		"--action=default=打开 DJOneHub",
	}
	if msg.Code != "" {
		title = fmt.Sprintf("验证码 %s · %s", msg.Code, displayNumber(msg.Sender))
		args = append(args, "--action=copy=复制验证码")
	}
	args = append(args, title, msg.Content)
	go func() {
		action, _, err := runNotify(args, nil)
		if err != nil {
			log.Printf("SMS notification: %v", err)
			return
		}
		switch action {
		case "copy":
			copyToClipboard(msg.Code)
		case "default":
			openURL(n.url)
		}
	}()
}

// runNotify runs notify-send with --print-id and actions (which imply --wait).
// onID fires as soon as the id is known, before the user acts.
func runNotify(args []string, onID func(uint32)) (action string, id uint32, err error) {
	hasPrintID := false
	for _, a := range args {
		if a == "--print-id" {
			hasPrintID = true
		}
	}
	if !hasPrintID {
		args = append([]string{"--print-id"}, args...)
	}
	cmd := exec.Command("notify-send", args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", 0, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", 0, err
	}
	scanner := bufio.NewScanner(out)
	if scanner.Scan() {
		if v, perr := strconv.ParseUint(strings.TrimSpace(scanner.Text()), 10, 32); perr == nil {
			id = uint32(v)
			if onID != nil {
				onID(id)
			}
		}
	}
	if scanner.Scan() {
		action = strings.TrimSpace(scanner.Text())
	}
	for scanner.Scan() {
	}
	if err := cmd.Wait(); err != nil {
		return action, id, fmt.Errorf("notify-send: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return action, id, nil
}

func notifySimple(title, body, icon string) {
	_ = exec.Command("notify-send", "--app-name="+notifyAppName, "--icon="+icon,
		"--hint=string:desktop-entry:"+desktopEntryID, title, body).Run()
}

func closeNotification(id uint32) {
	_ = exec.Command("gdbus", "call", "--session",
		"--dest", "org.freedesktop.Notifications",
		"--object-path", "/org/freedesktop/Notifications",
		"--method", "org.freedesktop.Notifications.CloseNotification",
		strconv.FormatUint(uint64(id), 10)).Run()
}

// loopRingtone plays the freedesktop phone ringtone until ctx is cancelled,
// with a safety cap so a lost hang-up can never ring forever.
func loopRingtone(ctx context.Context) {
	if _, err := os.Stat(ringtonePath); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "pw-cat", "--playback", "--media-role", "Notification", ringtonePath)
		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			log.Printf("ringtone: %v", err)
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func openURL(url string) {
	if err := exec.Command("xdg-open", url).Start(); err != nil {
		log.Printf("open %s: %v", url, err)
	}
}

func copyToClipboard(text string) {
	cmd := exec.Command("wl-copy")
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		cmd = exec.Command("xclip", "-selection", "clipboard")
	}
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		log.Printf("copy verification code: %v", err)
	}
}
