package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"syscall"
	"testing"
	"time"
)

type recordingRadio struct {
	messages [][]byte
}

func (r *recordingRadio) SendMessage(message []byte) error {
	r.messages = append(r.messages, append([]byte(nil), message...))
	return nil
}

func TestEncodeGAIAFrame(t *testing.T) {
	message := []byte{0x00, 0x02, 0x00, 0x23, 1, 2, 3}
	want := []byte{0xff, 0x01, 0x00, 0x03, 0x00, 0x02, 0x00, 0x23, 1, 2, 3}
	if got := encodeGAIAFrame(message); !bytes.Equal(got, want) {
		t.Fatalf("encodeGAIAFrame() = % x, want % x", got, want)
	}
}

func TestSatelliteMessageHasOfficial30ByteBody(t *testing.T) {
	message := encodeSatelliteInfoMessage("Gpredict")
	if len(message) != 34 {
		t.Fatalf("message length = %d, want 34", len(message))
	}
	if binary.BigEndian.Uint16(message[:2]) != 2 || binary.BigEndian.Uint16(message[2:4]) != 77 {
		t.Fatalf("unexpected command header: % x", message[:4])
	}
	if got := string(bytes.TrimRight(message[4:24], "\x00")); got != "Gpredict" {
		t.Fatalf("satellite label = %q, want Gpredict", got)
	}
	if !bytes.Equal(message[24:], make([]byte, 10)) {
		t.Fatalf("tracking tail = % x, want ten zero bytes", message[24:])
	}
}

func TestFrequencyUpdatesRequire50kHzChangePerVFO(t *testing.T) {
	radio := &recordingRadio{}
	const rxBase, txBase = uint32(145_800_000), uint32(435_000_000)
	control := NewFrequencyControl(radio, rxBase, txBase)
	if err := control.ApplyCurrentFrequencies(); err != nil {
		t.Fatal(err)
	}
	if len(radio.messages) != 2 {
		t.Fatalf("startup messages = %d, want 2", len(radio.messages))
	}

	if err := control.SetRxHz(rxBase + 25_000); err != nil {
		t.Fatal(err)
	}
	if err := control.SetTxHz(txBase + 49_999); err != nil {
		t.Fatal(err)
	}
	if len(radio.messages) != 2 {
		t.Fatalf("messages below threshold = %d, want startup pair only", len(radio.messages))
	}
	if control.RXHz() != rxBase+25_000 || control.TXHz() != txBase+49_999 {
		t.Fatalf("cached frequencies = %d/%d; small changes should still be cached", control.RXHz(), control.TXHz())
	}

	// The RX movement accumulated to exactly 50 kHz from the last sent value.
	if err := control.SetRxHz(rxBase + 50_000); err != nil {
		t.Fatal(err)
	}
	if len(radio.messages) != 4 {
		t.Fatalf("messages after exact RX threshold = %d, want 4", len(radio.messages))
	}
	if got := binary.BigEndian.Uint32(radio.messages[3][4:8]); got != rxBase+50_000 {
		t.Fatalf("sent RX = %d, want %d", got, rxBase+50_000)
	}
	if got := binary.BigEndian.Uint32(radio.messages[3][8:12]); got != txBase+49_999 {
		t.Fatalf("sent TX = %d, want latest cached %d", got, txBase+49_999)
	}

	// The RX update also sent the latest cached TX value. TX crosses its own
	// threshold only after moving 50 kHz from that transmitted value.
	if err := control.SetTxHz(txBase + 99_999); err != nil {
		t.Fatal(err)
	}
	if len(radio.messages) != 6 {
		t.Fatalf("messages after exact TX threshold = %d, want 6", len(radio.messages))
	}

}

func TestSPPChannelParser(t *testing.T) {
	sdp := `Service Name: Serial Port
Service Class ID List:
  "Serial Port" (0x1101)
Protocol Descriptor List:
  "L2CAP" (0x0100)
  "RFCOMM" (0x0003)
    Channel: 7
`
	if got := parseSPPChannel(sdp); got != 7 {
		t.Fatalf("parseSPPChannel() = %d, want 7", got)
	}
	if got := parseSPPChannel("Inquiring ...\nInquiry failed\n"); got != 0 {
		t.Fatalf("parseSPPChannel() on failed inquiry = %d, want 0", got)
	}
}

func TestEncodeFrequencyModeLayout(t *testing.T) {
	message, err := encodeFreqModeMessage(145_800_000, 435_000_000, 0x0a03)
	if err != nil {
		t.Fatal(err)
	}
	if len(message) != 20 || binary.BigEndian.Uint16(message[0:2]) != 2 || binary.BigEndian.Uint16(message[2:4]) != 35 {
		t.Fatalf("unexpected command layout: % x", message)
	}
	if binary.BigEndian.Uint32(message[4:8]) != 145_800_000 || binary.BigEndian.Uint32(message[8:12]) != 435_000_000 {
		t.Fatalf("unexpected RX/TX values: % x", message[4:12])
	}
	if binary.BigEndian.Uint16(message[16:18]) != 0x0a03 || binary.BigEndian.Uint16(message[18:20]) != 25_000 {
		t.Fatalf("unexpected mode flags/step: % x", message[16:20])
	}
}

func TestRadioReadLoopAccountsForHeaderBodyAndChecksum(t *testing.T) {
	server, client := net.Pipe()
	radio := NewRadio(server, false)
	defer radio.Close()
	defer client.Close()
	first := []byte{0xff, 0x01, 0x01, 0x01, 0x00, 0x02, 0x80, 0x04, 0x03, 0xaa}
	second := []byte{0xff, 0x01, 0x00, 0x01, 0x00, 0x02, 0x80, 0x04, 0x03}
	go func() { _ = writeFull(client, append(first, second...)) }()
	for i := 0; i < 2; i++ {
		if !radio.WaitDevInfoReply(time.Second) {
			t.Fatalf("did not parse GET_DEV_INFO reply %d", i+1)
		}
	}
}

func TestParseBluetoothAddressUsesLinuxByteOrder(t *testing.T) {
	got, err := parseBluetoothAddress("38:D2:00:01:2F:3A")
	if err != nil {
		t.Fatal(err)
	}
	want := [6]byte{0x3a, 0x2f, 0x01, 0x00, 0xd2, 0x38}
	if got != want {
		t.Fatalf("parsed address = % x, want % x", got, want)
	}
}

func TestRetryInterruptedConnect(t *testing.T) {
	calls := 0
	errno := retryConnectSyscall(func() syscall.Errno {
		calls++
		if calls < 3 {
			return syscall.EINTR
		}
		return 0
	})
	if errno != 0 || calls != 3 {
		t.Fatalf("retry result = %v after %d calls, want success after 3 calls", errno, calls)
	}
}

func TestRetryInterruptedConnectIsBounded(t *testing.T) {
	calls := 0
	errno := retryConnectSyscall(func() syscall.Errno {
		calls++
		return syscall.EINTR
	})
	if errno != syscall.EINTR || calls != maxConnectInterruptRetries {
		t.Fatalf("retry result = %v after %d calls, want EINTR after %d calls", errno, calls, maxConnectInterruptRetries)
	}
}

func TestWaitForRFCOMMConnectOnReadySocket(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	if err := waitForRFCOMMConnect(fds[0], time.Second); err != nil {
		t.Fatalf("already-connected stream was not recognized as ready: %v", err)
	}
}

func TestRFCOMMConnReadWrite(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := &rfcommConn{fd: fds[0]}
	defer conn.Close()
	defer syscall.Close(fds[1])

	if _, err := conn.Write([]byte("radio")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := syscall.Read(fds[1], buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "radio" {
		t.Fatalf("peer received %q, want radio", buf)
	}
	if _, err := syscall.Write(fds[1], []byte("reply")); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "reply" {
		t.Fatalf("connection read %q, want reply", buf)
	}
}

func TestRadioCloseUnblocksRFCOMMReadLoop(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := &rfcommConn{fd: fds[0]}
	defer conn.Close()
	defer syscall.Close(fds[1])

	radio := NewRadio(conn, false)
	// The peer sends nothing, so readLoop blocks waiting for the first frame byte.
	time.Sleep(20 * time.Millisecond)
	if err := radio.Close(); err != nil {
		t.Fatalf("radio.Close() failed: %v", err)
	}

	select {
	case <-radio.done:
	case <-time.After(time.Second):
		t.Fatal("radio readLoop remained blocked after disconnect")
	}
}

func TestRigctlSetAndGetRXFrequency(t *testing.T) {
	radio := &recordingRadio{}
	control := NewFrequencyControl(radio, 145_800_000, 435_000_000)
	protocol := NewRigctlProtocol(control)
	if got := protocol.Dispatch("F 145799000"); got != "RPRT 0\n" {
		t.Fatalf("set reply = %q, want RPRT 0", got)
	}
	if got := protocol.Dispatch("f"); got != "145799000\n" {
		t.Fatalf("frequency reply = %q, want 145799000", got)
	}
	messagesBeforePTTQueries := len(radio.messages)
	if got := protocol.Dispatch("t"); got != "0\n" {
		t.Fatalf("PTT status reply = %q, want 0 (RX/off)", got)
	}
	if got := protocol.Dispatch("T 1"); got != "RPRT -1\n" {
		t.Fatalf("PTT set reply = %q, want unsupported RPRT -1", got)
	}
	if len(radio.messages) != messagesBeforePTTQueries {
		t.Fatal("PTT query/set unexpectedly sent a radio command")
	}
}

func TestRigctlTransmitCommandsUpdateEncodedUplink(t *testing.T) {
	for _, command := range []string{
		"I 435001000",
		"\\set_split_freq 435001000",
		"\\set_freq TX 435001000",
	} {
		t.Run(command, func(t *testing.T) {
			radio := &recordingRadio{}
			control := NewFrequencyControl(radio, 437_800_000, 145_990_000)
			protocol := NewRigctlProtocol(control)
			if got := protocol.Dispatch(command); got != "RPRT 0\n" {
				t.Fatalf("dispatch reply = %q, want RPRT 0", got)
			}
			if got := control.TXHz(); got != 435_001_000 {
				t.Fatalf("TX state = %d, want 435001000", got)
			}
			if got := protocol.Dispatch("i"); got != "435001000\n" {
				t.Fatalf("split-frequency readback = %q, want 435001000", got)
			}
			if len(radio.messages) != 2 {
				t.Fatalf("sent %d messages, want satellite-info + frequency-mode pair", len(radio.messages))
			}
			message := radio.messages[1]
			if got := binary.BigEndian.Uint16(message[2:4]); got != commandFreqMode {
				t.Fatalf("second message command = %d, want FREQ_MODE_SET_PAR (%d)", got, commandFreqMode)
			}
			if got := binary.BigEndian.Uint32(message[4:8]); got != 437_800_000 {
				t.Fatalf("encoded RX frequency = %d, want 437800000", got)
			}
			if got := binary.BigEndian.Uint32(message[8:12]); got != 435_001_000 {
				t.Fatalf("encoded TX frequency = %d, want 435001000", got)
			}
		})
	}
}
