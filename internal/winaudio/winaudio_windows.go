//go:build windows

// Package winaudio is a minimal, dependency-free WASAPI shared-mode wrapper.
//
// It exists so the Windows build can move call audio between the module and
// this machine's own microphone and speakers, which is what audio_darwin.go
// does with CoreAudio on macOS. Only the pieces a two-way voice path needs are
// bound: endpoint enumeration, a shared capture stream and a shared render
// stream.
//
// Everything is plain syscall/vtable work, so the Windows binary keeps building
// with CGO_ENABLED=0 exactly as it does today.
package winaudio

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Flow selects the direction of an endpoint.
type Flow uint32

const (
	// Render is a playback endpoint (speakers, or a module's "speaker" side).
	Render Flow = 0
	// Capture is a recording endpoint (microphone, or a module's "microphone"
	// side, which would carry the remote party's downlink voice).
	Capture Flow = 1
)

const (
	clsctxAll                = 0x17
	deviceStateActive        = 0x1
	stgmRead                 = 0x0
	shareModeShared          = 0
	streamFlagsEventCallback = 0x00040000

	waveFormatIEEEFloat     = 3
	waveFormatExtensibleTag = 0xFFFE

	refTimesPerMilli = 10000 // REFERENCE_TIME is in 100 ns units.
)

var (
	clsidMMDeviceEnumerator = windows.GUID{Data1: 0xBCDE0395, Data2: 0xE52F, Data3: 0x467C, Data4: [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator  = windows.GUID{Data1: 0xA95664D2, Data2: 0x9614, Data3: 0x4F35, Data4: [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioClient         = windows.GUID{Data1: 0x1CB9AD4C, Data2: 0xDBFA, Data3: 0x4C32, Data4: [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidIAudioCaptureClient  = windows.GUID{Data1: 0xC8ADBD64, Data2: 0xE71E, Data3: 0x48A0, Data4: [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}
	iidIAudioRenderClient   = windows.GUID{Data1: 0xF294ACFC, Data2: 0x3146, Data3: 0x4483, Data4: [8]byte{0xA7, 0xBF, 0xAD, 0xDC, 0xA7, 0xC2, 0x60, 0xE2}}
	iidIDeviceTopology      = windows.GUID{Data1: 0x2A07407E, Data2: 0x6497, Data3: 0x4A18, Data4: [8]byte{0x97, 0x87, 0x32, 0xF7, 0x9B, 0xD0, 0xD9, 0x8F}}

	subtypeIEEEFloat = windows.GUID{Data1: 0x00000003, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}

	// PKEY_Device_FriendlyName
	pkeyDeviceFriendlyName = propertyKey{
		fmtid: windows.GUID{Data1: 0xA45C254E, Data2: 0xDF1C, Data3: 0x4EFD, Data4: [8]byte{0x80, 0x20, 0x67, 0xD1, 0x46, 0xA8, 0x50, 0xE0}},
		pid:   14,
	}

	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")

	comInitOnce sync.Once
)

type propertyKey struct {
	fmtid windows.GUID
	pid   uint32
}

// propVariant is the 64-bit PROPVARIANT layout; only the VT_LPWSTR case is
// read. val is typed as a pointer rather than a uintptr so the string it owns
// stays visible to the garbage collector and vet's unsafe.Pointer rules hold.
type propVariant struct {
	vt         uint16
	wReserved1 uint16
	wReserved2 uint16
	wReserved3 uint16
	val        *uint16
	_          uintptr
}

type waveFormatEx struct {
	wFormatTag      uint16
	nChannels       uint16
	nSamplesPerSec  uint32
	nAvgBytesPerSec uint32
	nBlockAlign     uint16
	wBitsPerSample  uint16
	cbSize          uint16
}

// WAVEFORMATEXTENSIBLE is WAVEFORMATEX (18 packed bytes) followed by a WORD, a
// DWORD and a GUID. It is deliberately NOT declared as a Go struct: waveFormatEx
// contains uint32 fields, so Go pads it to 20 bytes and every field after it
// would be read two bytes off — which silently makes SubFormat unreadable, so
// IEEE-float streams get decoded as 32-bit integers and turn into loud noise.
const subFormatOffset = 18 + 2 + 4 // sizeof(WAVEFORMATEX) + wValidBitsPerSample + dwChannelMask

func waveSubFormat(w *waveFormatEx) windows.GUID {
	return *(*windows.GUID)(unsafe.Add(unsafe.Pointer(w), subFormatOffset))
}

type comObject struct {
	vtbl *uintptr
}

func (o *comObject) call(index int, args ...uintptr) uintptr {
	slot := *(*uintptr)(unsafe.Pointer(uintptr(unsafe.Pointer(o.vtbl)) + uintptr(index)*unsafe.Sizeof(uintptr(0))))
	full := append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)
	ret, _, _ := syscall.SyscallN(slot, full...)
	return ret
}

func (o *comObject) release() {
	if o != nil {
		o.call(2)
	}
}

func hr(code uintptr, what string) error {
	if int32(code) >= 0 {
		return nil
	}
	return fmt.Errorf("%s: HRESULT 0x%08X", what, uint32(code))
}

// InitCOM initialises COM for the calling OS thread. Audio streams must be
// created and pumped from a thread that has done this; each pump goroutine
// locks its OS thread and calls it once.
func InitCOM() {
	// COINIT_MULTITHREADED keeps the pumps free of a message loop requirement.
	_ = windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED)
}

func initProcess() { comInitOnce.Do(InitCOM) }

// Format describes a PCM stream layout.
type Format struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	IsFloat       bool
}

func (f Format) blockAlign() int { return f.Channels * f.BitsPerSample / 8 }

func (f Format) String() string {
	kind := "int"
	if f.IsFloat {
		kind = "float"
	}
	return fmt.Sprintf("%dHz/%dch/%s%d", f.SampleRate, f.Channels, kind, f.BitsPerSample)
}

func formatFromWave(w *waveFormatEx) Format {
	f := Format{
		SampleRate:    int(w.nSamplesPerSec),
		Channels:      int(w.nChannels),
		BitsPerSample: int(w.wBitsPerSample),
	}
	switch w.wFormatTag {
	case waveFormatIEEEFloat:
		f.IsFloat = true
	case waveFormatExtensibleTag:
		f.IsFloat = waveSubFormat(w) == subtypeIEEEFloat
	}
	return f
}

// Device identifies one audio endpoint.
type Device struct {
	ID   string
	Name string
	Flow Flow
	// HardwareID is the upstream device path this endpoint is wired to, as
	// reported by the device topology — for a USB audio device it looks like
	// `{2}.\\?\usb#vid_2c7c&pid_0125&mi_05#...`. Binding on this rather than on
	// Name is what identifies a specific piece of hardware; it is empty when
	// the topology cannot be walked.
	HardwareID string
}

// MatchesUSB reports whether the endpoint is wired to the given USB vendor and
// product. Matching is done on the hardware path, never on the endpoint name,
// so an unrelated device that happens to be called "USB Audio" cannot be
// mistaken for the module.
//
// Two spellings are accepted because the path depends on how the machine routes
// USB audio:
//
//	usb#vid_2c7c&pid_0125&mi_05#...                 plain USB enumeration
//	...\intcusbtopocapture_b8412c7c01250500_0       Intel USB audio offload,
//	                                                which concatenates the ids
//
// The second form was found on a machine whose USB audio arrives through the
// Intel audio controller topology rather than as a bare USB endpoint.
func (d Device) MatchesUSB(vendorID, productID uint16) bool {
	if d.HardwareID == "" {
		return false
	}
	path := strings.ToLower(d.HardwareID)
	if strings.Contains(path, fmt.Sprintf("vid_%04x&pid_%04x", vendorID, productID)) {
		return true
	}
	return strings.Contains(path, fmt.Sprintf("%04x%04x", vendorID, productID))
}

// endpointHardwareID walks an endpoint's topology to the device it connects to.
// A failure is not fatal: callers fall back to weaker identification.
func endpointHardwareID(dev *comObject) string {
	var topology *comObject
	// IMMDevice::Activate(IDeviceTopology)
	if ret := dev.call(3,
		uintptr(unsafe.Pointer(&iidIDeviceTopology)),
		clsctxAll,
		0,
		uintptr(unsafe.Pointer(&topology)),
	); int32(ret) < 0 {
		return ""
	}
	defer topology.release()

	var count uint32
	// IDeviceTopology::GetConnectorCount
	if ret := topology.call(3, uintptr(unsafe.Pointer(&count))); int32(ret) < 0 || count == 0 {
		return ""
	}
	var connector *comObject
	// IDeviceTopology::GetConnector
	if ret := topology.call(4, 0, uintptr(unsafe.Pointer(&connector))); int32(ret) < 0 {
		return ""
	}
	defer connector.release()

	var raw *uint16
	// IConnector::GetDeviceIdConnectedTo
	if ret := connector.call(10, uintptr(unsafe.Pointer(&raw))); int32(ret) < 0 || raw == nil {
		return ""
	}
	id := windows.UTF16PtrToString(raw)
	windows.CoTaskMemFree(unsafe.Pointer(raw))
	return id
}

func enumerator() (*comObject, error) {
	initProcess()
	var ptr *comObject
	ret, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)),
		0,
		clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&ptr)),
	)
	if err := hr(ret, "CoCreateInstance(MMDeviceEnumerator)"); err != nil {
		return nil, err
	}
	return ptr, nil
}

func deviceFriendlyName(dev *comObject) string {
	var store *comObject
	// IMMDevice::OpenPropertyStore
	if ret := dev.call(4, stgmRead, uintptr(unsafe.Pointer(&store))); int32(ret) < 0 {
		return ""
	}
	defer store.release()

	var pv propVariant
	// IPropertyStore::GetValue
	if ret := store.call(5, uintptr(unsafe.Pointer(&pkeyDeviceFriendlyName)), uintptr(unsafe.Pointer(&pv))); int32(ret) < 0 {
		return ""
	}
	if pv.val == nil {
		return ""
	}
	name := windows.UTF16PtrToString(pv.val)
	windows.CoTaskMemFree(unsafe.Pointer(pv.val))
	return name
}

func deviceID(dev *comObject) string {
	var raw *uint16
	// IMMDevice::GetId
	if ret := dev.call(5, uintptr(unsafe.Pointer(&raw))); int32(ret) < 0 || raw == nil {
		return ""
	}
	id := windows.UTF16PtrToString(raw)
	windows.CoTaskMemFree(unsafe.Pointer(raw))
	return id
}

func describe(dev *comObject, flow Flow) Device {
	return Device{
		ID:         deviceID(dev),
		Name:       deviceFriendlyName(dev),
		Flow:       flow,
		HardwareID: endpointHardwareID(dev),
	}
}

// Enumerate lists the active endpoints for a direction.
func Enumerate(flow Flow) ([]Device, error) {
	enum, err := enumerator()
	if err != nil {
		return nil, err
	}
	defer enum.release()

	var collection *comObject
	// IMMDeviceEnumerator::EnumAudioEndpoints
	if err := hr(enum.call(3, uintptr(flow), deviceStateActive, uintptr(unsafe.Pointer(&collection))), "EnumAudioEndpoints"); err != nil {
		return nil, err
	}
	defer collection.release()

	var count uint32
	// IMMDeviceCollection::GetCount
	if err := hr(collection.call(3, uintptr(unsafe.Pointer(&count))), "GetCount"); err != nil {
		return nil, err
	}

	devices := make([]Device, 0, count)
	for i := uint32(0); i < count; i++ {
		var dev *comObject
		// IMMDeviceCollection::Item
		if ret := collection.call(4, uintptr(i), uintptr(unsafe.Pointer(&dev))); int32(ret) < 0 {
			continue
		}
		devices = append(devices, describe(dev, flow))
		dev.release()
	}
	return devices, nil
}

// Default returns the endpoint Windows uses for communications on this flow,
// falling back to the console default when no communications device is set.
func Default(flow Flow) (Device, error) {
	enum, err := enumerator()
	if err != nil {
		return Device{}, err
	}
	defer enum.release()

	for _, role := range []uintptr{2 /* eCommunications */, 0 /* eConsole */} {
		var dev *comObject
		// IMMDeviceEnumerator::GetDefaultAudioEndpoint
		if ret := enum.call(4, uintptr(flow), role, uintptr(unsafe.Pointer(&dev))); int32(ret) < 0 {
			continue
		}
		out := describe(dev, flow)
		dev.release()
		if out.ID != "" {
			return out, nil
		}
	}
	return Device{}, errors.New("no default audio endpoint")
}

func openDevice(id string) (*comObject, error) {
	enum, err := enumerator()
	if err != nil {
		return nil, err
	}
	defer enum.release()

	wide, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return nil, err
	}
	var dev *comObject
	// IMMDeviceEnumerator::GetDevice
	if err := hr(enum.call(5, uintptr(unsafe.Pointer(wide)), uintptr(unsafe.Pointer(&dev))), "GetDevice"); err != nil {
		return nil, err
	}
	return dev, nil
}

type stream struct {
	device  *comObject
	client  *comObject
	service *comObject
	event   windows.Handle
	format  Format
	frame   int // bytes per frame
	bufFrm  uint32
	started bool
	mu      sync.Mutex
}

func (s *stream) closeLocked() {
	if s.started {
		s.client.call(11) // IAudioClient::Stop
		s.started = false
	}
	if s.service != nil {
		s.service.release()
		s.service = nil
	}
	if s.client != nil {
		s.client.release()
		s.client = nil
	}
	if s.device != nil {
		s.device.release()
		s.device = nil
	}
	if s.event != 0 {
		windows.CloseHandle(s.event)
		s.event = 0
	}
}

// Format reports the stream's negotiated PCM layout.
func (s *stream) Format() Format { return s.format }

func activateClient(dev *comObject) (*comObject, error) {
	var client *comObject
	// IMMDevice::Activate
	if err := hr(dev.call(3,
		uintptr(unsafe.Pointer(&iidIAudioClient)),
		clsctxAll,
		0,
		uintptr(unsafe.Pointer(&client)),
	), "Activate(IAudioClient)"); err != nil {
		return nil, err
	}
	return client, nil
}

func mixFormat(client *comObject) (*waveFormatEx, error) {
	var wf *waveFormatEx
	// IAudioClient::GetMixFormat
	if err := hr(client.call(8, uintptr(unsafe.Pointer(&wf))), "GetMixFormat"); err != nil {
		return nil, err
	}
	return wf, nil
}

func initClient(client *comObject, wf *waveFormatEx, bufferMillis int, event windows.Handle) error {
	duration := int64(bufferMillis) * refTimesPerMilli
	// IAudioClient::Initialize
	if err := hr(client.call(3,
		shareModeShared,
		uintptr(streamFlagsEventCallback),
		uintptr(duration),
		0,
		uintptr(unsafe.Pointer(wf)),
		0,
	), "IAudioClient::Initialize"); err != nil {
		return err
	}
	// IAudioClient::SetEventHandle
	return hr(client.call(13, uintptr(event)), "SetEventHandle")
}

// prepare fills s in place. A stream carries a mutex, so it is never copied
// by value after construction.
func prepare(s *stream, dev Device, bufferMillis int) error {
	device, err := openDevice(dev.ID)
	if err != nil {
		return err
	}
	client, err := activateClient(device)
	if err != nil {
		device.release()
		return err
	}
	wf, err := mixFormat(client)
	if err != nil {
		client.release()
		device.release()
		return err
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(wf))

	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		client.release()
		device.release()
		return err
	}
	if err := initClient(client, wf, bufferMillis, event); err != nil {
		windows.CloseHandle(event)
		client.release()
		device.release()
		return err
	}

	s.device = device
	s.client = client
	s.event = event
	s.format = formatFromWave(wf)
	s.frame = s.format.blockAlign()
	// IAudioClient::GetBufferSize
	if err := hr(client.call(4, uintptr(unsafe.Pointer(&s.bufFrm))), "GetBufferSize"); err != nil {
		s.closeLocked()
		return err
	}
	return nil
}

func (s *stream) bindService(iid *windows.GUID, what string) error {
	// IAudioClient::GetService
	if err := hr(s.client.call(14, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&s.service))), what); err != nil {
		s.closeLocked()
		return err
	}
	// IAudioClient::Start
	if err := hr(s.client.call(10), "IAudioClient::Start"); err != nil {
		s.closeLocked()
		return err
	}
	s.started = true
	return nil
}

// CaptureStream reads PCM frames from an endpoint.
type CaptureStream struct{ stream }

// OpenCapture starts a shared-mode capture stream using the endpoint's own mix
// format. bufferMillis sizes the engine buffer.
func OpenCapture(dev Device, bufferMillis int) (*CaptureStream, error) {
	c := &CaptureStream{}
	if err := prepare(&c.stream, dev, bufferMillis); err != nil {
		return nil, err
	}
	if err := c.bindService(&iidIAudioCaptureClient, "GetService(IAudioCaptureClient)"); err != nil {
		return nil, err
	}
	return c, nil
}

// Read drains every queued packet and appends the frames to dst as interleaved
// float32 samples, returning the appended slice and the frame count. Silent
// packets expand to zeros so timing stays correct.
func (c *CaptureStream) Read(dst []float32, waitMillis uint32) ([]float32, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.service == nil {
		return dst, 0, errors.New("capture stream closed")
	}
	if waitMillis > 0 {
		windows.WaitForSingleObject(c.event, waitMillis)
	}

	frames := 0
	for {
		var packet uint32
		// IAudioCaptureClient::GetNextPacketSize
		if ret := c.service.call(5, uintptr(unsafe.Pointer(&packet))); int32(ret) < 0 {
			return dst, frames, hr(ret, "GetNextPacketSize")
		}
		if packet == 0 {
			return dst, frames, nil
		}

		var data *byte
		var avail, flags uint32
		// IAudioCaptureClient::GetBuffer
		ret := c.service.call(3,
			uintptr(unsafe.Pointer(&data)),
			uintptr(unsafe.Pointer(&avail)),
			uintptr(unsafe.Pointer(&flags)),
			0, 0,
		)
		if int32(ret) < 0 {
			return dst, frames, hr(ret, "IAudioCaptureClient::GetBuffer")
		}
		if avail > 0 {
			n := int(avail) * c.format.Channels
			base := len(dst)
			dst = append(dst, make([]float32, n)...)
			const silentFlag = 0x1
			if flags&silentFlag == 0 && data != nil {
				decodeInto(dst[base:base+n], unsafe.Slice(data, int(avail)*c.frame), c.format)
			}
			frames += int(avail)
		}
		// IAudioCaptureClient::ReleaseBuffer
		c.service.call(4, uintptr(avail))
	}
}

// Close stops and releases the stream.
func (c *CaptureStream) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

// RenderStream writes PCM frames to an endpoint.
type RenderStream struct{ stream }

// OpenRender starts a shared-mode render stream using the endpoint's mix format.
func OpenRender(dev Device, bufferMillis int) (*RenderStream, error) {
	r := &RenderStream{}
	if err := prepare(&r.stream, dev, bufferMillis); err != nil {
		return nil, err
	}
	if err := r.bindService(&iidIAudioRenderClient, "GetService(IAudioRenderClient)"); err != nil {
		return nil, err
	}
	return r, nil
}

// Available reports how many frames can be written without blocking.
func (r *RenderStream) Available() (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client == nil {
		return 0, errors.New("render stream closed")
	}
	var padding uint32
	// IAudioClient::GetCurrentPadding
	if ret := r.client.call(6, uintptr(unsafe.Pointer(&padding))); int32(ret) < 0 {
		return 0, hr(ret, "GetCurrentPadding")
	}
	return int(r.bufFrm - padding), nil
}

// Write pushes interleaved float32 samples and returns the frames actually
// written; the caller keeps any remainder for the next round.
func (r *RenderStream) Write(src []float32, waitMillis uint32) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.service == nil {
		return 0, errors.New("render stream closed")
	}
	if len(src) == 0 {
		return 0, nil
	}
	if waitMillis > 0 {
		windows.WaitForSingleObject(r.event, waitMillis)
	}

	var padding uint32
	if ret := r.client.call(6, uintptr(unsafe.Pointer(&padding))); int32(ret) < 0 {
		return 0, hr(ret, "GetCurrentPadding")
	}
	free := int(r.bufFrm - padding)
	if free <= 0 {
		return 0, nil
	}
	want := len(src) / r.format.Channels
	if want > free {
		want = free
	}
	if want == 0 {
		return 0, nil
	}

	var data *byte
	// IAudioRenderClient::GetBuffer
	if ret := r.service.call(3, uintptr(want), uintptr(unsafe.Pointer(&data))); int32(ret) < 0 {
		return 0, hr(ret, "IAudioRenderClient::GetBuffer")
	}
	encodeInto(unsafe.Slice(data, want*r.frame), src[:want*r.format.Channels], r.format)
	// IAudioRenderClient::ReleaseBuffer
	r.service.call(4, uintptr(want), 0)
	return want, nil
}

// Close stops and releases the stream.
func (r *RenderStream) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeLocked()
}

// decodeInto converts one WASAPI packet to float32 in [-1,1].
func decodeInto(dst []float32, raw []byte, f Format) {
	switch {
	case f.IsFloat && f.BitsPerSample == 32:
		copy(dst, unsafe.Slice((*float32)(unsafe.Pointer(&raw[0])), len(dst)))
	case !f.IsFloat && f.BitsPerSample == 16:
		for i := range dst {
			dst[i] = float32(int16(uint16(raw[i*2])|uint16(raw[i*2+1])<<8)) / 32768
		}
	case !f.IsFloat && f.BitsPerSample == 32:
		for i := range dst {
			v := int32(uint32(raw[i*4]) | uint32(raw[i*4+1])<<8 | uint32(raw[i*4+2])<<16 | uint32(raw[i*4+3])<<24)
			dst[i] = float32(v) / 2147483648
		}
	case !f.IsFloat && f.BitsPerSample == 24:
		for i := range dst {
			v := int32(uint32(raw[i*3])<<8 | uint32(raw[i*3+1])<<16 | uint32(raw[i*3+2])<<24)
			dst[i] = float32(v) / 2147483648
		}
	default:
		for i := range dst {
			dst[i] = 0
		}
	}
}

func clamp1(v float32) float32 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// encodeInto converts float32 in [-1,1] to the endpoint's PCM layout.
func encodeInto(raw []byte, src []float32, f Format) {
	switch {
	case f.IsFloat && f.BitsPerSample == 32:
		dst := unsafe.Slice((*float32)(unsafe.Pointer(&raw[0])), len(src))
		for i, v := range src {
			dst[i] = clamp1(v)
		}
	case !f.IsFloat && f.BitsPerSample == 16:
		for i, v := range src {
			s := int16(clamp1(v) * 32767)
			raw[i*2] = byte(uint16(s))
			raw[i*2+1] = byte(uint16(s) >> 8)
		}
	case !f.IsFloat && f.BitsPerSample == 32:
		for i, v := range src {
			u := uint32(int32(float64(clamp1(v)) * 2147483647))
			raw[i*4] = byte(u)
			raw[i*4+1] = byte(u >> 8)
			raw[i*4+2] = byte(u >> 16)
			raw[i*4+3] = byte(u >> 24)
		}
	case !f.IsFloat && f.BitsPerSample == 24:
		for i, v := range src {
			u := uint32(int32(float64(clamp1(v)) * 2147483647))
			raw[i*3] = byte(u >> 8)
			raw[i*3+1] = byte(u >> 16)
			raw[i*3+2] = byte(u >> 24)
		}
	default:
		for i := range raw {
			raw[i] = 0
		}
	}
}
