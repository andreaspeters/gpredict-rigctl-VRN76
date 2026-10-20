//go:build linux

// gpredict_vrn76_bridge is a Linux SPP/RFCOMM rigctld bridge for the VR-N76.
// It mirrors the default SPP transport and satellite-VFO behavior of the Python
// bridge. The optional BLE transport is intentionally not implemented here.
package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	frequencyUpdateThresholdHz  uint32 = 50
	defaultAddress                     = "38:D2:00:01:2F:3A"
	defaultRFCOMMChannel               = 1
	defaultHost                        = "127.0.0.1"
	defaultPort                        = 4532
	defaultRXHz                 uint32 = 437_800_000
	defaultTXHz                 uint32 = 145_990_000
	maxFrequency                uint32 = (1 << 30) - 1
	commandGroup                uint16 = 2
	commandDevInfo              uint16 = 4
	commandRegisterNotification uint16 = 6
	commandEventNotification    uint16 = 9
	commandReadSettings         uint16 = 10
	commandSatelliteInfo        uint16 = 77
	commandFreqMode             uint16 = 35
	flagsInitial                uint16 = 0x0A03
	flagsSteady                 uint16 = 0x0A00
	channelStep                 uint16 = 25_000
)

// encodeGAIAFrame wraps one command message in the serial GAIA frame.
func encodeGAIAFrame(message []byte) []byte {
	if len(message) < 4 || len(message)-4 > 255 {
		return nil
	}
	frame := make([]byte, 4+len(message))
	frame[0], frame[1], frame[2], frame[3] = 0xFF, 0x01, 0, byte(len(message)-4)
	copy(frame[4:], message)
	return frame
}

func encodeCommand(command uint16, body []byte) []byte {
	message := make([]byte, 4+len(body))
	binary.BigEndian.PutUint16(message[0:2], commandGroup)
	binary.BigEndian.PutUint16(message[2:4], command)
	copy(message[4:], body)
	return message
}

func encodeSatelliteInfoMessage(name string) []byte {
	nameBytes := []byte(name)
	if len(nameBytes) > 20 || !isASCII(nameBytes) {
		return nil
	}
	body := make([]byte, 30)
	copy(body, nameBytes)
	return encodeCommand(commandSatelliteInfo, body)
}

func isASCII(value []byte) bool {
	for _, b := range value {
		if b > 0x7F {
			return false
		}
	}
	return true
}

func encodeFreqModeMessage(rxHz, txHz uint32, flags uint16) ([]byte, error) {
	if rxHz == 0 || rxHz > maxFrequency || txHz == 0 || txHz > maxFrequency {
		return nil, fmt.Errorf("frequencies must be in [1,%d] Hz", maxFrequency)
	}
	body := make([]byte, 16)
	binary.BigEndian.PutUint32(body[0:4], rxHz)
	binary.BigEndian.PutUint32(body[4:8], txHz)
	binary.BigEndian.PutUint16(body[8:10], 0)  // RX sub-audio
	binary.BigEndian.PutUint16(body[10:12], 0) // TX sub-audio
	binary.BigEndian.PutUint16(body[12:14], flags)
	binary.BigEndian.PutUint16(body[14:16], channelStep)
	return encodeCommand(commandFreqMode, body), nil
}

func encodeRadioInitMessages() [][]byte {
	return [][]byte{
		encodeCommand(commandDevInfo, []byte{3}),
		encodeCommand(commandReadSettings, nil),
		encodeCommand(commandRegisterNotification, []byte{1, 14}),
	}
}

var channelPattern = regexp.MustCompile(`(?im)"RFCOMM"\s*\(0x0003\)[^\n]*\n\s*Channel:\s*(\d+)`)

// parseSPPChannel returns zero when output does not contain a valid SPP channel.
func parseSPPChannel(output string) int {
	if !regexp.MustCompile(`(?i)(Serial Port|0x1101)`).MatchString(output) {
		return 0
	}
	match := channelPattern.FindStringSubmatch(output)
	if len(match) != 2 {
		return 0
	}
	channel, err := strconv.Atoi(match[1])
	if err != nil || channel < 1 || channel > 30 {
		return 0
	}
	return channel
}

func resolveSPPChannel(address string, override int) (int, error) {
	if override != 0 {
		if override < 1 || override > 30 {
			return 0, fmt.Errorf("SPP RFCOMM channel must be between 1 and 30")
		}
		return override, nil
	}
	tool, err := exec.LookPath("sdptool")
	if err != nil {
		log.Printf("WARNING: sdptool unavailable; using SPP RFCOMM channel %d", defaultRFCOMMChannel)
		return defaultRFCOMMChannel, nil
	}
	output, runErr := exec.Command(tool, "search", "SP", address).CombinedOutput()
	if channel := parseSPPChannel(string(output)); channel != 0 {
		log.Printf("Resolved SPP RFCOMM channel %d via SDP", channel)
		return channel, nil
	}
	if runErr != nil {
		log.Printf("WARNING: SPP SDP lookup failed (%v): %s", runErr, strings.TrimSpace(string(output)))
	} else {
		log.Printf("WARNING: SPP service not resolved: %s", strings.TrimSpace(string(output)))
	}
	log.Printf("Trying SPP RFCOMM channel %d", defaultRFCOMMChannel)
	return defaultRFCOMMChannel, nil
}

// parseBluetoothAddress stores bdaddr bytes in the byte order expected by Linux.
func parseBluetoothAddress(address string) ([6]byte, error) {
	var result [6]byte
	parts := strings.Split(address, ":")
	if len(parts) != 6 {
		return result, fmt.Errorf("invalid Bluetooth address %q", address)
	}
	for i, part := range parts {
		if len(part) != 2 {
			return result, fmt.Errorf("invalid Bluetooth address %q", address)
		}
		value, err := strconv.ParseUint(part, 16, 8)
		if err != nil {
			return result, fmt.Errorf("invalid Bluetooth address %q: %w", address, err)
		}
		result[5-i] = byte(value)
	}
	return result, nil
}

type sockaddrRFCOMM struct {
	Family   uint16
	Address  [6]byte
	Channel  uint8
	Reserved uint8
}

const (
	maxConnectInterruptRetries = 8
	rfcommConnectTimeout       = 30 * time.Second
)

func retryConnectSyscall(connect func() syscall.Errno) syscall.Errno {
	for attempt := 0; attempt < maxConnectInterruptRetries; attempt++ {
		errno := connect()
		if errno != syscall.EINTR {
			return errno
		}
	}
	return syscall.EINTR
}

func waitForRFCOMMConnect(fd int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := waitForRFCOMMWritable(fd, deadline, timeout); err != nil {
			return err
		}
		socketErr, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ERROR)
		if err != nil {
			return fmt.Errorf("read RFCOMM connect status: %w", err)
		}
		if socketErr == 0 {
			return nil
		}
		if socketErr == int(syscall.EINPROGRESS) || socketErr == int(syscall.EALREADY) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		return syscall.Errno(socketErr)
	}
}

func waitForRFCOMMWritable(fd int, deadline time.Time, timeout time.Duration) error {
	for {
		var writable syscall.FdSet
		word := fd / 64
		if fd < 0 || word >= len(writable.Bits) {
			return fmt.Errorf("RFCOMM socket descriptor %d exceeds select capacity", fd)
		}
		writable.Bits[word] |= int64(1) << uint(fd%64)
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("RFCOMM connect timed out after %s", timeout)
		}
		timeval := syscall.NsecToTimeval(remaining.Nanoseconds())
		ready, err := syscall.Select(fd+1, nil, &writable, nil, &timeval)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("wait for RFCOMM connect: %w", err)
		}
		if ready == 0 {
			return fmt.Errorf("RFCOMM connect timed out after %s", timeout)
		}
		return nil
	}
}

func dialRFCOMM(address string, channel int) (radioConn, error) {
	if channel < 1 || channel > 30 {
		return nil, fmt.Errorf("RFCOMM channel must be between 1 and 30")
	}
	bdaddr, err := parseBluetoothAddress(address)
	if err != nil {
		return nil, err
	}
	const (
		afBluetooth   = 31
		btProtoRFCOMM = 3
	)
	// Match benlink's working Python path: create a nonblocking RFCOMM socket,
	// then wait for connect readiness. A blocking RawSyscall is interruptible
	// (EINTR) and bypasses Go's scheduler-aware syscall path.
	fd, err := syscall.Socket(afBluetooth, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, btProtoRFCOMM)
	if err != nil {
		return nil, fmt.Errorf("open Bluetooth RFCOMM socket: %w", err)
	}
	sockaddr := sockaddrRFCOMM{Family: afBluetooth, Address: bdaddr, Channel: uint8(channel)}
	if err := connectRFCOMM(fd, &sockaddr); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("connect RFCOMM %s channel %d: %w", address, channel, err)
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("set connected RFCOMM socket to blocking mode: %w", err)
	}
	return &rfcommConn{fd: fd}, nil
}

func connectRFCOMM(fd int, sockaddr *sockaddrRFCOMM) error {
	errno := retryConnectSyscall(func() syscall.Errno {
		_, _, errno := syscall.Syscall(
			syscall.SYS_CONNECT,
			uintptr(fd),
			uintptr(unsafe.Pointer(sockaddr)),
			uintptr(unsafe.Sizeof(*sockaddr)),
		)
		runtime.KeepAlive(sockaddr)
		return errno
	})
	if errno == syscall.EINPROGRESS || errno == syscall.EALREADY || errno == syscall.EAGAIN {
		return waitForRFCOMMConnect(fd, rfcommConnectTimeout)
	}
	if errno != 0 && errno != syscall.EISCONN {
		return errno
	}
	return nil
}

type radioConn interface {
	io.ReadWriteCloser
	SetWriteDeadline(time.Time) error
}

type rfcommConn struct {
	fd            int
	mu            sync.Mutex
	writeDeadline time.Time
	closeOnce     sync.Once
}

func (c *rfcommConn) Read(p []byte) (int, error) {
	for {
		n, err := syscall.Read(c.fd, p)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		return n, err
	}
}

func (c *rfcommConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	deadline := c.writeDeadline
	c.mu.Unlock()
	var timeout syscall.Timeval
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		timeout = syscall.NsecToTimeval(remaining.Nanoseconds())
	}
	if err := syscall.SetsockoptTimeval(c.fd, syscall.SOL_SOCKET, syscall.SO_SNDTIMEO, &timeout); err != nil {
		return 0, fmt.Errorf("set RFCOMM send timeout: %w", err)
	}
	for {
		n, err := syscall.Write(c.fd, p)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		return n, err
	}
}

func (c *rfcommConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		// Shutdown the socket with SHUT_RDWR to wake up any blocking reads before closing
		syscall.Shutdown(c.fd, syscall.SHUT_RDWR)
		err = syscall.Close(c.fd)
	})
	return err
}

func (c *rfcommConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.writeDeadline = deadline
	c.mu.Unlock()
	return nil
}

type MessageSender interface {
	SendMessage([]byte) error
}

type FrequencyControl struct {
	mu          sync.Mutex
	radio       MessageSender
	rxHz        uint32
	txHz        uint32
	updates     int
	lastRxHz    uint32
	lastTxHz    uint32
	hasSentPair bool
}

func NewFrequencyControl(radio MessageSender, rxHz, txHz uint32) *FrequencyControl {
	return &FrequencyControl{radio: radio, rxHz: rxHz, txHz: txHz}
}

func (c *FrequencyControl) RXHz() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rxHz
}

func (c *FrequencyControl) TXHz() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.txHz
}

func (c *FrequencyControl) ApplyCurrentFrequencies() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendSatellitePair(c.rxHz, c.txHz, false)
}

func (c *FrequencyControl) SetRxHz(frequency uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if frequency == 0 || frequency > maxFrequency {
		return fmt.Errorf("RX frequency must be in [1,%d] Hz", maxFrequency)
	}
	if err := c.sendSatellitePair(frequency, c.txHz, true); err != nil {
		return err
	}
	c.rxHz = frequency
	return nil
}

func (c *FrequencyControl) SetTxHz(frequency uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if frequency == 0 || frequency > maxFrequency {
		return fmt.Errorf("TX frequency must be in [1,%d] Hz", maxFrequency)
	}
	if err := c.sendSatellitePair(c.rxHz, frequency, true); err != nil {
		return err
	}
	c.txHz = frequency
	return nil
}

func (c *FrequencyControl) sendSatellitePair(rxHz, txHz uint32, countUpdate bool) error {
	// ApplyCurrentFrequencies seeds the radio on connect; subsequent rigctl
	// updates are sent only when either VFO moves at least 50 kHz from the
	// last pair actually sent. Small changes accumulate in rxHz/txHz.
	if countUpdate && c.hasSentPair &&
		frequencyDelta(c.lastRxHz, rxHz) < frequencyUpdateThresholdHz &&
		frequencyDelta(c.lastTxHz, txHz) < frequencyUpdateThresholdHz {
		return nil
	}

	flags := flagsSteady
	if c.updates < 2 {
		flags = flagsInitial
	}
	if err := c.radio.SendMessage(encodeSatelliteInfoMessage("Gpredict")); err != nil {
		return err
	}
	message, err := encodeFreqModeMessage(rxHz, txHz, flags)
	if err != nil {
		return err
	}
	if err := c.radio.SendMessage(message); err != nil {
		return err
	}
	if countUpdate {
		c.updates++
	}

	// Update the cached last sent frequencies
	c.lastRxHz = rxHz
	c.lastTxHz = txHz
	c.hasSentPair = true

	return nil
}

func frequencyDelta(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

type RigctlProtocol struct {
	control      *FrequencyControl
	mu           sync.Mutex
	splitEnabled bool
	txVFO        string
}

func NewRigctlProtocol(control *FrequencyControl) *RigctlProtocol {
	return &RigctlProtocol{control: control, splitEnabled: control.RXHz() != control.TXHz(), txVFO: "VFOB"}
}

var rigctlCommandHandlers = map[string]func(*RigctlProtocol, []string) string{
	"f":                (*RigctlProtocol).handleGetFreq,
	"i":                (*RigctlProtocol).handleGetSplitFreq,
	"\\get_split_freq": (*RigctlProtocol).handleGetSplitFreq,
	"t":                (*RigctlProtocol).handleGetPTT,
	"\\get_freq":       (*RigctlProtocol).handleGetFreqByVFO,
	"F":                (*RigctlProtocol).handleSetFreq,
	"I":                (*RigctlProtocol).handleSetSplitFreq,
	"\\set_split_freq": (*RigctlProtocol).handleSetSplitFreq,
	"\\set_freq":       (*RigctlProtocol).handleSetFreqByVFO,
	"s":                (*RigctlProtocol).handleGetSplit,
	"S":                (*RigctlProtocol).handleSetSplit,
}

func (p *RigctlProtocol) Dispatch(line string) string {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return "RPRT -1\n"
	}
	handler, ok := rigctlCommandHandlers[fields[0]]
	if !ok {
		return "RPRT -1\n"
	}
	return handler(p, fields)
}

// handleGetFreq handles the "f" command
func (p *RigctlProtocol) handleGetFreq(fields []string) string {
	if len(fields) != 1 {
		return "RPRT -1\n"
	}
	return fmt.Sprintf("%d\n", p.control.RXHz())
}

// handleGetSplitFreq handles the "i" and "\\get_split_freq" commands
func (p *RigctlProtocol) handleGetSplitFreq(fields []string) string {
	if len(fields) != 1 {
		return "RPRT -1\n"
	}
	return fmt.Sprintf("%d\n", p.control.TXHz())
}

// handleGetPTT handles the "t" command
func (p *RigctlProtocol) handleGetPTT(fields []string) string {
	if len(fields) != 1 {
		return "RPRT -1\n"
	}
	// Report RX/off for Hamlib's read-only PTT query. No transmit control
	// is implemented; the corresponding setter remains unsupported.
	return "0\n"
}

// handleGetFreqByVFO handles the "\\get_freq" command
func (p *RigctlProtocol) handleGetFreqByVFO(fields []string) string {
	if len(fields) > 2 {
		return "RPRT -1\n"
	}
	vfo := "RX"
	if len(fields) == 2 {
		vfo = strings.ToUpper(fields[1])
	}
	switch vfo {
	case "RX", "VFO", "VFOA", "A":
		return fmt.Sprintf("%d\n", p.control.RXHz())
	case "TX", "VFOB", "B":
		return fmt.Sprintf("%d\n", p.control.TXHz())
	default:
		return "RPRT -1\n"
	}
}

// parseFrequency is a helper for parsing frequency values
func (p *RigctlProtocol) parseFrequency(value string) (uint32, error) {
	v, err := strconv.ParseUint(value, 10, 32)
	if err != nil || v == 0 || v > uint64(maxFrequency) {
		return 0, fmt.Errorf("invalid frequency")
	}
	return uint32(v), nil
}

// handleSetFreq handles the "F" command
func (p *RigctlProtocol) handleSetFreq(fields []string) string {
	if len(fields) != 2 {
		return "RPRT -1\n"
	}
	frequency, err := p.parseFrequency(fields[1])
	if err != nil {
		return "RPRT -1\n"
	}
	if err := p.control.SetRxHz(frequency); err != nil {
		return "RPRT -1\n"
	}
	return "RPRT 0\n"
}

// handleSetSplitFreq handles the "I" and "\\set_split_freq" commands
func (p *RigctlProtocol) handleSetSplitFreq(fields []string) string {
	if len(fields) != 2 {
		return "RPRT -1\n"
	}
	frequency, err := p.parseFrequency(fields[1])
	if err != nil {
		return "RPRT -1\n"
	}
	if err := p.control.SetTxHz(frequency); err != nil {
		return "RPRT -1\n"
	}
	return "RPRT 0\n"
}

// handleSetFreqByVFO handles the "\\set_freq" command
func (p *RigctlProtocol) handleSetFreqByVFO(fields []string) string {
	var vfo, value string
	if len(fields) == 2 {
		vfo, value = "RX", fields[1]
	} else if len(fields) == 3 {
		vfo, value = strings.ToUpper(fields[1]), fields[2]
	} else {
		return "RPRT -1\n"
	}
	frequency, err := p.parseFrequency(value)
	if err != nil {
		return "RPRT -1\n"
	}
	switch vfo {
	case "RX", "VFO", "VFOA", "A":
		if err := p.control.SetRxHz(frequency); err != nil {
			return "RPRT -1\n"
		}
	case "TX", "VFOB", "B":
		if err := p.control.SetTxHz(frequency); err != nil {
			return "RPRT -1\n"
		}
	default:
		return "RPRT -1\n"
	}
	return "RPRT 0\n"
}

// handleGetSplit handles the "s" command
func (p *RigctlProtocol) handleGetSplit(fields []string) string {
	if len(fields) != 1 {
		return "RPRT -1\n"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	enabled := 0
	if p.splitEnabled {
		enabled = 1
	}
	return fmt.Sprintf("%d\n%s\n", enabled, p.txVFO)
}

// handleSetSplit handles the "S" command
func (p *RigctlProtocol) handleSetSplit(fields []string) string {
	if len(fields) != 3 || (fields[1] != "0" && fields[1] != "1") {
		return "RPRT -1\n"
	}
	vfo := strings.ToUpper(fields[2])
	switch vfo {
	case "VFOA", "VFOB", "VFOC", "CURRVFO", "VFO", "MEM", "MAIN", "SUB", "TX", "RX":
	default:
		return "RPRT -1\n"
	}
	p.mu.Lock()
	p.splitEnabled = fields[1] == "1"
	p.txVFO = vfo
	p.mu.Unlock()
	return "RPRT 0\n"
}

type Radio struct {
	conn         radioConn
	verbose      bool
	writeMu      sync.Mutex
	devInfoReply chan struct{}
	done         chan struct{}
	closeOnce    sync.Once
}

func NewRadio(conn radioConn, verbose bool) *Radio {
	r := &Radio{conn: conn, verbose: verbose, devInfoReply: make(chan struct{}, 1), done: make(chan struct{})}
	go r.readLoop()
	return r
}

func (r *Radio) SendMessage(message []byte) error {
	frame := encodeGAIAFrame(message)
	if frame == nil {
		return errors.New("invalid GAIA message")
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if err := r.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("set radio write timeout: %w", err)
	}
	defer r.conn.SetWriteDeadline(time.Time{})
	if r.verbose {
		log.Printf("CLASSIC-RAW -> command bytes=%x", frame)
	}
	if err := writeFull(r.conn, frame); err != nil {
		return err
	}
	if r.verbose {
		log.Printf("Radio command write completed")
	}
	return nil
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (r *Radio) IsConnected() bool {
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

func (r *Radio) WaitDevInfoReply(timeout time.Duration) bool {
	select {
	case <-r.devInfoReply:
		return true
	case <-r.done:
		return false
	case <-time.After(timeout):
		return false
	}
}

func (r *Radio) Close() error {
	var err error
	r.closeOnce.Do(func() { err = r.conn.Close() })
	return err
}

func (r *Radio) readLoop() {
	defer close(r.done)
	reader := bufio.NewReader(r.conn)
	for {
		flags, length, err := readGAIAFrameHeader(reader)
		if err != nil {
			return
		}
		payload, err := readGAIAPayload(reader, flags, length)
		if err != nil {
			return
		}
		r.handleMessage(flags, payload)
	}
}

func readGAIAFrameHeader(reader *bufio.Reader) (byte, byte, error) {
	for {
		start, err := reader.ReadByte()
		if err != nil {
			return 0, 0, err
		}
		if start != 0xFF {
			continue
		}
		version, err := reader.ReadByte()
		if err != nil {
			return 0, 0, err
		}
		if version != 0x01 {
			continue
		}
		var header [2]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return 0, 0, err
		}
		return header[0], header[1], nil
	}
}

func readGAIAPayload(reader *bufio.Reader, flags, length byte) ([]byte, error) {
	// GAIA's one-byte length counts only the message body; the four-byte
	// group/reply/command header is still present on the wire.
	payload := make([]byte, 4+int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	if flags&0x01 != 0 { // GAIA CHECKSUM flag adds one trailing checksum byte.
		if _, err := reader.ReadByte(); err != nil {
			return nil, err
		}
	}
	return payload, nil
}

func (r *Radio) handleMessage(frameFlags byte, message []byte) {
	if len(message) < 4 {
		return
	}
	group := binary.BigEndian.Uint16(message[0:2])
	commandWord := binary.BigEndian.Uint16(message[2:4])
	isReply := commandWord&0x8000 != 0
	command := commandWord & 0x7FFF
	body := message[4:]
	if r.verbose {
		log.Printf("Radio <- group=%d cmd=%d reply=%t frame_flags=0x%02x body=%x", group, command, isReply, frameFlags, body)
	}
	if group != commandGroup {
		return
	}
	if isReply {
		r.handleReply(command, body)
		return
	}
	if command == commandEventNotification && len(body) > 0 && body[0] == 14 {
		decodeFreqModeStatus(body[1:])
	}
}

func (r *Radio) handleReply(command uint16, body []byte) {
	if command == commandDevInfo {
		select {
		case r.devInfoReply <- struct{}{}:
		default:
		}
	}
	if command == commandSatelliteInfo || command == commandFreqMode {
		status := "<empty>"
		if len(body) != 0 {
			status = fmt.Sprintf("0x%02x", body[0])
		}
		log.Printf("Radio reply cmd=%d status=%s body=%x", command, status, body)
	}
}

func decodeFreqModeStatus(payload []byte) {
	if len(payload) < 14 {
		log.Printf("WARNING: malformed FREQ_MODE_STATUS_CHANGED event (14): %x", payload)
		return
	}
	rxHz := binary.BigEndian.Uint32(payload[0:4]) & maxFrequency
	txHz := binary.BigEndian.Uint32(payload[4:8]) & maxFrequency
	flags := binary.BigEndian.Uint16(payload[12:14])
	log.Printf("Radio-confirmed VFO: active=%t RX=%d Hz TX=%d Hz flags=0x%04x", payload[13] != 0, rxHz, txHz, flags)
}

func sendRadioInitSequence(radio *Radio) error {
	messages := encodeRadioInitMessages()
	for i, message := range messages {
		if i == 2 && !radio.WaitDevInfoReply(2*time.Second) {
			log.Printf("WARNING: GET_DEV_INFO reply timed out; sending notification subscription anyway")
		}
		log.Printf("Sending HTCommander initialization command %d/%d", i+1, len(messages))
		if err := radio.SendMessage(message); err != nil {
			return err
		}
		if !radio.IsConnected() {
			return errors.New("radio disconnected during initialization")
		}
		if i < len(messages)-1 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	return nil
}

func handleClient(conn net.Conn, protocol *RigctlProtocol, control *FrequencyControl, verbose bool) {
	defer conn.Close()
	log.Printf("Gpredict/rigctld client connected: %s", conn.RemoteAddr())
	if err := control.ApplyCurrentFrequencies(); err != nil {
		log.Printf("ERROR: could not activate satellite VFO for %s: %v", conn.RemoteAddr(), err)
		return
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if verbose {
			log.Printf("rigctld <- %s", line)
		}
		response := protocol.Dispatch(line)
		if verbose {
			log.Printf("rigctld -> %s", strings.TrimSpace(response))
		}
		if err := writeFull(conn, []byte(response)); err != nil {
			if verbose {
				log.Printf("client write failed: %v", err)
			}
			return
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("WARNING: rigctld client read failed: %v", err)
	}
	log.Printf("Gpredict/rigctld client closed: %s", conn.RemoteAddr())
}

func openRadioTransport(address string, channel int, verbose bool) (*Radio, error) {
	resolvedChannel, err := resolveSPPChannel(address, channel)
	if err != nil {
		return nil, err
	}
	log.Printf("Opening raw RFCOMM socket for SPP channel %d", resolvedChannel)
	conn, err := dialRFCOMM(address, resolvedChannel)
	if err != nil {
		return nil, err
	}
	return NewRadio(conn, verbose), nil
}

func runBridge(address, host string, port int, rxHz, txHz uint32, channel int, verbose bool) error {
	radio, err := openRadioTransport(address, channel, verbose)
	if err != nil {
		return err
	}
	defer radio.Close()
	log.Printf("Radio transport ready (connected=%t)", radio.IsConnected())
	if err := sendRadioInitSequence(radio); err != nil {
		return err
	}
	control := NewFrequencyControl(radio, rxHz, txHz)
	log.Printf("Setting satellite VFO once at startup: RX=%d Hz TX=%d Hz", rxHz, txHz)
	if err := control.ApplyCurrentFrequencies(); err != nil {
		return err
	}
	log.Printf("Initial satellite VFO command pair sent")

	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("listen on %s:%d: %w", host, port, err)
	}
	defer listener.Close()
	log.Printf("rigctld-compatible server listening on %s", listener.Addr())
	protocol := NewRigctlProtocol(control)
	for {
		client, err := listener.Accept()
		if err != nil {
			return err
		}
		go handleClient(client, protocol, control, verbose)
	}
}

func main() {
	address := flag.String("address", defaultAddress, "VR-N76 Bluetooth address")
	host := flag.String("host", defaultHost, "TCP bind host")
	port := flag.Int("port", defaultPort, "rigctld TCP port")
	rxHz := flag.Uint("rx-frequency", uint(defaultRXHz), "initial satellite RX frequency in Hz")
	txHz := flag.Uint("tx-frequency", uint(defaultTXHz), "initial satellite TX frequency in Hz")
	transport := flag.String("transport", "classic-raw", "radio link (classic-raw/classic use SPP RFCOMM; BLE unsupported)")
	channel := flag.Int("rfcomm-channel", 0, "override SPP RFCOMM channel discovery (1-30)")
	verbose := flag.Bool("verbose", false, "enable debug logging")
	headless := flag.Bool("headless", false, "run without the Qt window and connect immediately")
	flag.Parse()
	if *verbose {
		log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	}
	if *transport != "classic-raw" && *transport != "classic" {
		log.Fatal("this Go build supports only Bluetooth Classic SPP/RFCOMM; --transport ble is not implemented")
	}
	if *rxHz > uint(maxFrequency) || *txHz > uint(maxFrequency) {
		log.Fatal("frequencies must be in [1, 1073741823] Hz")
	}
	if *headless {
		if err := runBridge(*address, *host, *port, uint32(*rxHz), uint32(*txHz), *channel, *verbose); err != nil {
			log.Fatal(err)
		}
		return
	}
	controller, err := newBridgeController(*address, *host, *port, uint32(*rxHz), uint32(*txHz), *channel, *verbose)
	if err != nil {
		log.Fatal(err)
	}
	defer controller.Close()
	runBridgeGUI(controller)
}
