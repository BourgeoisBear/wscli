// CLI for websocket interaction
package main

import (
	"io"
	"net/http"
	"sync"
	"time"

	gws "github.com/gorilla/websocket"
)

// HandlerErr contains package sentinel errors
type HandlerErr int

const (
	ErrNilWs HandlerErr = iota
)

const (
	TextMessage   = gws.TextMessage
	BinaryMessage = gws.BinaryMessage
	CloseMessage  = gws.CloseMessage
	PingMessage   = gws.PingMessage
	PongMessage   = gws.PongMessage
)

func (err HandlerErr) Error() string {
	switch err {
	case ErrNilWs:
		return "nil websocket connection"
	}
	return "unknown HandlerErr"
}

type Handler struct {
	wsConn              *gws.Conn
	dlRead, dlWrite     time.Duration
	chRdrErr, chPingErr chan error

	// signal completion from goroutines
	chRdrDone, chPingDone chan struct{}

	// signal completion from caller
	chCloseAll chan struct{}

	mtxWri sync.Mutex // setup + write mutex
}

type MsgReaderFunc func(pHdl *Handler, nType int, iRdr io.Reader, err error) bool

// Dial returns a new websocket.Conn for the given websocket URL.
func Dial(url string, hdr http.Header) (*gws.Conn, *http.Response, error) {
	return gws.DefaultDialer.Dial(url, hdr)
}

// GetConn returns the Handler's underlying websocket.Conn.
func (ss *Handler) GetConn() *gws.Conn {
	return ss.wsConn
}

// Wait for Handler completion.
func (ss *Handler) Wait() (errRdr, errPing error) {
	// wait for readpump completion, capture exit err
	if ss.chRdrDone != nil {
		<-ss.chRdrDone
		errRdr = <-ss.chRdrErr
		ss.chRdrDone = nil
		ss.chRdrErr = nil
	}
	// wait for pingpump completion, capture exit err
	if ss.chPingDone != nil {
		<-ss.chPingDone
		errPing = <-ss.chPingErr
		ss.chPingDone = nil
		ss.chPingErr = nil
	}
	// clean up
	ss.chCloseAll = nil
	ss.wsConn = nil
	return
}

// Close Handler (without waiting for completion).
func (ss *Handler) Close() error {
	if ss.chCloseAll != nil {
		close(ss.chCloseAll)
	}
	if ss.wsConn == nil {
		return ErrNilWs
	}

	// in case deadlines aren't in use
	return ss.wsConn.Close()
}

// WriteMessage to the Handler's websocket
func (ss *Handler) WriteMessage(nType int, bsMsg []byte) error {

	if ss.wsConn == nil {
		return ErrNilWs
	}

	ss.mtxWri.Lock()
	defer ss.mtxWri.Unlock()

	if ss.dlWrite > 0 {
		if err := ss.wsConn.SetWriteDeadline(time.Now().Add(ss.dlWrite)); err != nil {
			return err
		}
	}
	return ss.wsConn.WriteMessage(nType, bsMsg)
}

// StartHandler creates 'read' and 'ping' message-pump goroutines
func StartHandler(
	pwsConn *gws.Conn,
	pingInterval, readTimeout, writeTimeout time.Duration,
	fnHandleMsg MsgReaderFunc,
) (*Handler, error) {

	if pwsConn == nil {
		return nil, ErrNilWs
	}

	// housekeeping
	ss := new(Handler)
	ss.wsConn = pwsConn

	ss.dlRead = readTimeout
	ss.dlWrite = writeTimeout

	ss.chCloseAll = make(chan struct{})
	ss.chRdrDone = make(chan struct{})
	ss.chRdrErr = make(chan error, 1)

	// read pump
	go func() {

		var err error
		defer func() {
			close(ss.chRdrDone)
			ss.chRdrErr <- err
		}()

		for {
			select {
			case <-ss.chCloseAll:
				return
			case <-ss.chPingDone:
				// stop on ping pump termination
				return
			default:
				if ss.dlRead > 0 {
					if err = ss.wsConn.SetReadDeadline(time.Now().Add(ss.dlRead)); err != nil {
						return
					}
				}
				var mtype int
				var iRdr io.Reader
				mtype, iRdr, err = ss.wsConn.NextReader()
				if !fnHandleMsg(ss, mtype, iRdr, err) {
					return
				}
			}
		}
	}()

	// ping ticker
	if pingInterval > 0 {

		ss.chPingDone = make(chan struct{})
		ss.chPingErr = make(chan error, 1)

		go func() {

			ticker := time.NewTicker(pingInterval)

			var err error
			defer func() {
				ticker.Stop()
				close(ss.chPingDone)
				ss.chPingErr <- err
			}()

			for {
				select {
				case <-ss.chCloseAll:
					return
				case <-ss.chRdrDone:
					// stop on read pump termination
					return
				case _ = <-ticker.C:
					if err = ss.WriteMessage(gws.PingMessage, nil); err != nil {
						return
					}
				}
			}
		}()
	}

	// mutexed writer for ping handler + deadline updating for recvd ping msg
	pwsConn.SetPingHandler(func(_ string) error {
		if err := ss.WriteMessage(gws.PongMessage, nil); err != nil {
			return err
		}
		if readTimeout > 0 {
			return pwsConn.SetReadDeadline(time.Now().Add(readTimeout))
		}
		return nil
	})

	return ss, nil
}
