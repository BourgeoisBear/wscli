package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"time"
)

/*
	TODO:
		- message filtering
		- \log change command
		- readline + history
*/

func fnErrAbort(prfx string, err error) {
	errfunc(prfx, err, true)
}

func fnErr(prfx string, err error) {
	errfunc(prfx, err, false)
}

func errfunc(prfx string, err error, bAbort bool) {
	const SA = "\x1b[91m"
	const SZ = "\x1b[0m"

	if err != nil {
		if len(prfx) > 0 {
			fmt.Fprintln(os.Stderr, SA+strings.ToUpper(prfx)+": "+err.Error()+SZ)
		} else {
			fmt.Fprintln(os.Stderr, SA+err.Error()+SZ)
		}
		if bAbort {
			os.Exit(-1)
		}
	}
}

type fsm struct {
	pH       *Handler
	hdr      http.Header
	msg      []byte
	msgEnd   []byte
	rxMsgEnd *regexp.Regexp
	printTs  bool
	logWri   io.Writer
}

func newFsm() *fsm {
	return &fsm{
		hdr:      make(http.Header),
		msg:      make([]byte, 0, 4096),
		rxMsgEnd: regexp.MustCompile(`\\msg\s+(.*)`),
	}
}

func (pf *fsm) closeWait() {
	if pf.pH != nil {
		err := pf.pH.Close()
		fnErr("ws close", err)

		pf.pH.Wait()
		pf.pH = nil
	}
	pf.resetMsg()
}

func (pf *fsm) resetMsg() {
	pf.msg = pf.msg[:0]
	pf.msgEnd = nil
}

func (pf *fsm) processLine(line []byte) {

	// panic recover
	defer func() {
		if r := recover(); r != nil {
			stk := debug.Stack()
			fmt.Fprintf(os.Stderr, "\x1b[91;1mPANIC: %v\x1b[0m\n", r)
			lines := bytes.Split(stk, []byte{'\n'})
			for _, ln := range lines {
				fmt.Fprint(os.Stderr, "\t", string(ln), "\n")
			}
		}
	}()

	if pf.msgEnd != nil {

		// fmt.Printf("TERMINATOR %#v\n", string(pf.msgEnd))

		if bytes.Equal(line, pf.msgEnd) {

			// fmt.Printf("SEND %#v\n", string(pf.msg))
			if pf.pH != nil {
				err := pf.pH.WriteMessage(TextMessage, pf.msg)
				if err != nil {
					fnErr("WS WRITE", err)
					return
				}
				fmt.Print("\x1b[96m", "SENT", "\x1b[0m\n")
			}

			pf.resetMsg()

		} else {

			// fmt.Printf("APPEND %#v\n", string(line))
			pf.msg = append(pf.msg, line...)
		}

		return
	}

	switch {

	// begin message heredoc
	case bytes.HasPrefix(line, []byte("\\msg")):
		pf.resetMsg()
		sMtch := pf.rxMsgEnd.FindSubmatch(line)
		if len(sMtch) < 2 {
			pf.msgEnd = nil
		} else {
			pf.msgEnd = make([]byte, len(sMtch[1]))
			copy(pf.msgEnd, sMtch[1])
		}
		pf.msgEnd = append(pf.msgEnd, '\n')

	// clear headers
	case bytes.HasPrefix(line, []byte("\\hdrclr")):
		pf.hdr = make(http.Header)

	// list headers
	case bytes.HasPrefix(line, []byte("\\hdrlst")):
		for k := range pf.hdr {
			fmt.Print(
				"\t\x1b[93m",
				k,
				":\x1b[0m ",
				fmt.Sprintf("%v", pf.hdr[k]),
				"\n",
			)
		}

	// hang-up
	case bytes.HasPrefix(line, []byte("\\hup")):
		pf.closeWait()

	// dial
	case bytes.HasPrefix(line, []byte("\\dial ws")):

		line = bytes.TrimPrefix(line, []byte("\\dial "))
		if line = bytes.TrimSpace(line); len(line) == 0 {
			return
		}
		pf.closeWait()
		pC, wsRsp, err := Dial(string(line), pf.hdr)
		if err != nil {
			fnErr("WS DIAL ["+wsRsp.Status+"]", err)
			return
		}
		pf.pH, err = StartHandler(pC, 10*time.Second, 0, 0, pf.PrintMsg)
		if err != nil {
			fnErr("WS HANDLER", err)
			return
		}

	// add/remove HTTP headers
	case len(line) > 0:

		// parse & add header
		parts := bytes.SplitN(line, []byte(":"), 2)
		if len(parts) < 2 {
			return
		}

		// get key
		hk := string(bytes.TrimSpace(parts[0]))
		if len(hk) == 0 {
			return
		}

		// get value
		// del from map on empty, otherwise add
		hv := string(bytes.TrimSpace(parts[1]))
		if len(hv) == 0 {
			pf.hdr.Del(hk)
		} else {
			pf.hdr.Add(hk, hv)
		}
	}
}

// websocket -> stdout
func (pf *fsm) PrintMsg(pHdl *Handler, _ int, iRdr io.Reader, err error) bool {

	// exit on websocket error
	if err != nil {
		return false
	}

	// read full response
	bsMsg, err := io.ReadAll(iRdr)
	if err != nil {
		fnErr("websocket->stdout", err)
		return false
	}

	// write to stdout
	if len(bsMsg) > 0 {

		// header
		szFrom := ""
		if pC := pHdl.GetConn(); pC != nil {
			szFrom = pC.RemoteAddr().String()
		}

		if pf.printTs {

			// sender + timestamp
			now := time.Now().Round(0)
			fmt.Printf(
				"\x1b[37m%s [%s]:\x1b[0m\n",
				now.Format(time.RFC3339),
				szFrom,
			)

			// color message
			fmt.Print("\x1b[92m")
		}

		// raw output
		if pf.logWri != nil {
			pf.logWri.Write(bsMsg)
		}
		os.Stdout.Write(bsMsg)

		// reset color
		if pf.printTs {
			fmt.Print("\x1b[0m")
		}
	}

	return true
}

func main() {

	const helpPrefix = `wscli
	Command-line interface to a websocket connection.
	Ctrl-c to exit.

COMMANDS

	Connect to Websocket
		\dial ws://127.0.0.1:8080/ws

	Disconnect from Websocket
		\hup

	Send Message (heredoc format)
		\msg [message terminator]
		message line 1
		message line 2
		message line 3
		[message terminator]

		(default terminator is a blank line if left unspecified)

	Specify HTTP Headers
		Authorization: awo875pu84uj6paj436up
		Content-Type: application/json

	List Specified HTTP Headers
		\hdrlst

	Clear Specific HTTP Header (key without value)
		Authorization:

	Clear All Specified HTTP Headers
		\hdrclr

`

	// HELP MESSAGE
	var iWriFlag io.Writer = os.Stdout
	flag.CommandLine.SetOutput(iWriFlag)
	flag.Usage = func() {
		fmt.Fprint(iWriFlag, helpPrefix)
		flag.PrintDefaults()
		fmt.Fprint(iWriFlag, "\n")
	}

	fsm := newFsm()
	var szLogPath string
	flag.BoolVar(&fsm.printTs, "ts", false, "print message timestamps")
	flag.StringVar(&szLogPath, "log", "-", "output log file")
	flag.Parse()

	if szLogPath != "-" {
		pfLog, eLog := os.OpenFile(szLogPath, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0664)
		if eLog != nil {
			fnErrAbort("file open", eLog)
			return
		}
		fsm.logWri = pfLog
		defer pfLog.Close()
	}

	buf := make([]byte, 4096)

	if wsConn := flag.Arg(0); (len(wsConn) > 0) && strings.HasPrefix(wsConn, "ws") {
		fsm.processLine([]byte("\\dial " + wsConn))
	}

	for {

		n, rdErr := os.Stdin.Read(buf)
		if rdErr != nil && rdErr != io.EOF {
			fnErrAbort("stdin read", rdErr)
			break
		}

		if n > 0 {
			fsm.processLine(buf[0:n])
		}

		// fifo without a writer does not block, so throttle just in case
		if rdErr == io.EOF {
			time.Sleep(time.Second / 2)
		}
	}
}
