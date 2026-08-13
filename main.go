package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---------- Конфигурация ----------
const (
	switchIP = "192.168.121.2"
	password = "123asd456" // оставлен в коде по вашему желанию

	loginCallCmd  = 123
	toggleCallCmd = 103

	httpTimeout = 10 * time.Second
)

// ---------- WinAPI константы ----------
const (
	wsOverlapped = 0x00000000
	wsCaption    = 0x00C00000
	wsSysMenu    = 0x00080000
	wsMinimize   = 0x00020000
	wsVisible    = 0x10000000
	wsChild      = 0x40000000

	bsAutoRadioButton = 0x00000009
	bsPushButton      = 0x00000000

	wmCreate  = 0x0001
	wmCommand = 0x0111
	wmDestroy = 0x0002
	wmApp     = 0x8000

	bnClicked = 0

	swShow = 5

	idApply  = 1000
	idStatus = 1001

	idPort1  = 2001
	idAllOff = 2009

	windowWidth  = 550
	windowHeight = 420
	windowX      = 300
	windowY      = 200

	radioLeft   = 20
	radioWidth  = 150
	radioHeight = 28
	radioOffset = 35

	applyLeft   = 200
	applyTop    = 100
	applyWidth  = 150
	applyHeight = 40

	statusLeft   = 200
	statusTop    = 160
	statusWidth  = 300
	statusHeight = 80
)

// ---------- Структуры для WinAPI ----------
type point struct {
	X int32
	Y int32
}

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	Menu       uintptr
	ClassName  *uint16
	IconSm     uintptr
}

// ---------- Глобальные переменные GUI ----------
var (
	mainWindow   uintptr
	statusWindow uintptr
	applyButton  uintptr

	selectedPort int
	mu           sync.Mutex

	cancelCtx  context.Context
	cancelFunc context.CancelFunc
)

// ---------- Вспомогательные функции WinAPI ----------
var (
	user32 = windows.NewLazySystemDLL("user32.dll")

	procCreateWindowEx  = user32.NewProc("CreateWindowExW")
	procDefWindowProc   = user32.NewProc("DefWindowProcW")
	procDestroyWindow   = user32.NewProc("DestroyWindow")
	procShowWindow      = user32.NewProc("ShowWindow")
	procUpdateWindow    = user32.NewProc("UpdateWindow")
	procRegisterClass   = user32.NewProc("RegisterClassExW")
	procGetMessage      = user32.NewProc("GetMessageW")
	procTranslateMsg    = user32.NewProc("TranslateMessage")
	procDispatchMessage = user32.NewProc("DispatchMessageW")
	procSetWindowText   = user32.NewProc("SetWindowTextW")
	procEnableWindow    = user32.NewProc("EnableWindow")
	procSendMessage     = user32.NewProc("SendMessageW")
	procPostQuitMessage = user32.NewProc("PostQuitMessage")
	procGetModuleHandle = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
)

func utf16Ptr(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

func createControl(className, text string, style uint32, x, y, w, h int, id int, parent uintptr) uintptr {
	class := utf16Ptr(className)
	txt := utf16Ptr(text)

	hwnd, _, _ := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(class)),
		uintptr(unsafe.Pointer(txt)),
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent,
		uintptr(id),
		0, 0,
	)
	return hwnd
}

func setText(hwnd uintptr, text string) {
	p := utf16Ptr(text)
	procSetWindowText.Call(hwnd, uintptr(unsafe.Pointer(p)))
}

func enable(hwnd uintptr, enabled bool) {
	var v uintptr
	if enabled {
		v = 1
	}
	procEnableWindow.Call(hwnd, v)
}

// ---------- Клиент для работы с коммутатором ----------
type SwitchClient struct {
	client  *http.Client
	baseURL string
	cookie  string
	mu      sync.Mutex
}

func NewSwitchClient(ip string) *SwitchClient {
	return &SwitchClient{
		client: &http.Client{
			Timeout: httpTimeout,
		},
		baseURL: "http://" + ip,
	}
}

func (c *SwitchClient) makePayload(callcmd int, calldata map[string]interface{}) ([]byte, error) {
	obj := map[string]interface{}{
		"data": map[string]interface{}{
			"callcmd":  callcmd,
			"calldata": calldata,
		},
	}
	return json.Marshal(obj)
}

func (c *SwitchClient) Login() error {
	body, err := c.makePayload(loginCallCmd, map[string]interface{}{
		"password": password,
	})
	if err != nil {
		return err
	}

	resp, err := c.client.Post(c.baseURL+"/123", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP статус %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var result struct {
		ErrCode int `json:"errcode"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if result.ErrCode != 0 {
		return fmt.Errorf("ошибка логина: errcode=%d", result.ErrCode)
	}

	var cookie string
	for _, ck := range resp.Cookies() {
		if ck.Name != "" {
			cookie = ck.Name + "=" + ck.Value
			break
		}
	}
	if cookie == "" {
		for _, h := range resp.Header.Values("Set-Cookie") {
			cookie = strings.TrimSpace(strings.Split(h, ";")[0])
			if cookie != "" {
				break
			}
		}
	}
	if cookie == "" {
		return fmt.Errorf("не удалось получить session cookie")
	}

	c.mu.Lock()
	c.cookie = cookie
	c.mu.Unlock()
	return nil
}

func (c *SwitchClient) opcodeOff(port int) int {
	return 16 * (8 - port)
}

func (c *SwitchClient) opcodeOn(port int) int {
	return c.opcodeOff(port) + 2560
}

func (c *SwitchClient) Toggle(port int, state bool) error {
	c.mu.Lock()
	cookie := c.cookie
	c.mu.Unlock()
	if cookie == "" {
		return fmt.Errorf("не выполнена авторизация")
	}

	opcode := c.opcodeOff(port)
	if state {
		opcode = c.opcodeOn(port)
	}

	body, err := c.makePayload(toggleCallCmd, map[string]interface{}{
		"opcode": opcode,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", c.baseURL+"/103", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", cookie)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP статус %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var result struct {
		ErrCode int `json:"errcode"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if result.ErrCode != 0 {
		return fmt.Errorf("ошибка переключения: errcode=%d", result.ErrCode)
	}
	return nil
}

// ---------- GUI логика ----------
func postStatusMessage(text string) {
	procSendMessage.Call(mainWindow, wmApp, 0, uintptr(unsafe.Pointer(utf16Ptr(text))))
}

func postEnableApply(enabled bool) {
	var v uintptr
	if enabled {
		v = 1
	}
	procSendMessage.Call(mainWindow, wmApp, 1, v)
}

func applySelection(ctx context.Context) {
	postEnableApply(false)
	postStatusMessage("Применение...")

	go func() {
		client := NewSwitchClient(switchIP)

		if err := client.Login(); err != nil {
			postStatusMessage("Ошибка login: " + err.Error())
			postEnableApply(true)
			return
		}

		for p := 1; p <= 8; p++ {
			select {
			case <-ctx.Done():
				postStatusMessage("Отмена")
				postEnableApply(true)
				return
			default:
			}
			if err := client.Toggle(p, false); err != nil {
				postStatusMessage(fmt.Sprintf("Ошибка OFF порт %d: %v", p, err))
				postEnableApply(true)
				return
			}
		}

		mu.Lock()
		port := selectedPort
		mu.Unlock()

		if port >= 1 && port <= 8 {
			if err := client.Toggle(port, true); err != nil {
				postStatusMessage(fmt.Sprintf("Ошибка ON порт %d: %v", port, err))
				postEnableApply(true)
				return
			}
			postStatusMessage(fmt.Sprintf("Готово: порт %d включён", port))
		} else {
			postStatusMessage("Готово: все порты выключены")
		}
		postEnableApply(true)
	}()
}

// ---------- Оконная процедура ----------
func wndProc(hwnd uintptr, msg uint32, wParam uintptr, lParam uintptr) uintptr {
	switch msg {
	case wmCreate:
		for i := 1; i <= 8; i++ {
			id := idPort1 + i - 1
			x := radioLeft
			y := 30 + (i-1)*radioOffset
			createControl(
				"BUTTON",
				fmt.Sprintf("Порт %d", i),
				wsChild|wsVisible|bsAutoRadioButton,
				x, y, radioWidth, radioHeight,
				id, hwnd,
			)
		}
		createControl(
			"BUTTON",
			"Все выключить",
			wsChild|wsVisible|bsAutoRadioButton,
			radioLeft, 320, radioWidth, radioHeight,
			idAllOff, hwnd,
		)
		applyButton = createControl(
			"BUTTON",
			"ПРИМЕНИТЬ",
			wsChild|wsVisible|bsPushButton,
			applyLeft, applyTop, applyWidth, applyHeight,
			idApply, hwnd,
		)
		statusWindow = createControl(
			"STATIC",
			"Не подключено",
			wsChild|wsVisible,
			statusLeft, statusTop, statusWidth, statusHeight,
			idStatus, hwnd,
		)
		return 0

	case wmCommand:
		id := int(wParam & 0xffff)
		code := uint32((wParam >> 16) & 0xffff)

		if code == bnClicked {
			if id >= idPort1 && id <= idPort1+7 {
				mu.Lock()
				selectedPort = id - idPort1 + 1
				mu.Unlock()
			}
			if id == idAllOff {
				mu.Lock()
				selectedPort = 0
				mu.Unlock()
			}
			if id == idApply {
				applySelection(cancelCtx)
			}
		}
		return 0

	case wmApp:
		if wParam == 0 && lParam != 0 {
			text := windows.UTF16PtrToString((*uint16)(unsafe.Pointer(lParam)))
			setText(statusWindow, text)
		} else if wParam == 1 {
			enabled := lParam != 0
			enable(applyButton, enabled)
		}
		return 0

	case wmDestroy:
		if cancelFunc != nil {
			cancelFunc()
		}
		procPostQuitMessage.Call(0)
		return 0
	}

	ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

// ---------- Точка входа ----------
func main() {
	className := utf16Ptr("SEVEN8GE")

	instance, _, _ := procGetModuleHandle.Call(0)

	wndProcPtr := windows.NewCallback(wndProc)

	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   wndProcPtr,
		Instance:  instance,
		ClassName: className,
	}

	procRegisterClass.Call(uintptr(unsafe.Pointer(&wc)))

	cancelCtx, cancelFunc = context.WithCancel(context.Background())

	mainWindow, _, _ = procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16Ptr("SEVEN 8GE"))),
		wsOverlapped|wsCaption|wsSysMenu|wsMinimize,
		windowX, windowY, windowWidth, windowHeight,
		0, 0, instance, 0,
	)

	if mainWindow == 0 {
		panic("не удалось создать главное окно")
	}

	procShowWindow.Call(mainWindow, swShow)
	procUpdateWindow.Call(mainWindow)

	var m msg
	for {
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if ret == 0 {
			break
		}
		procTranslateMsg.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMsg.Call(uintptr(unsafe.Pointer(&m)))
	}
}