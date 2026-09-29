package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

const (
	appTitle           = "B站链接转换器"
	configFileName     = "bilibili-url.json"
	th32csSnapProcess  = 0x00000002
	errorAlreadyExists = syscall.Errno(183)
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutexW             = kernel32.NewProc("CreateMutexW")
	procCloseHandle              = kernel32.NewProc("CloseHandle")
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW          = kernel32.NewProc("Process32FirstW")
	procProcess32NextW           = kernel32.NewProc("Process32NextW")
	bvPathPattern                = regexp.MustCompile(`(?i)^/video/(BV[0-9A-Za-z]{10})/?$`)
	b23LinkPattern               = regexp.MustCompile(`(?i)(?:https?://)?(?:www\.)?b23\.tv/[0-9A-Za-z_-]+`)
	embeddedBilibiliLinkPattern  = regexp.MustCompile(`(?i)(?:^|[^0-9A-Za-z.-])((?:https?://)?(?:[0-9A-Za-z-]+\.)*bilibili\.com/video/BV[0-9A-Za-z]{10}/?)`)
)

type conversionRule struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Template string `json:"template"`
}

type appSettings struct {
	Enabled               bool             `json:"enabled"`
	OnlyWhenVRChatRunning bool             `json:"onlyWhenVRChatRunning"`
	ActiveRuleID          string           `json:"activeRuleId"`
	Rules                 []conversionRule `json:"rules"`
}

type processEntry32 struct {
	Size              uint32
	Usage             uint32
	ProcessID         uint32
	DefaultHeapID     uintptr
	ModuleID          uint32
	Threads           uint32
	ParentProcessID   uint32
	PriorityClassBase int32
	Flags             uint32
	ExeFile           [260]uint16
}

type ruleTableModel struct {
	walk.TableModelBase
	settings *appSettings
}

func (model *ruleTableModel) RowCount() int {
	return len(model.settings.Rules)
}

func (model *ruleTableModel) Value(row, column int) interface{} {
	rule := model.settings.Rules[row]
	switch column {
	case 0:
		if strings.EqualFold(rule.ID, model.settings.ActiveRuleID) {
			return "当前"
		}
		return ""
	case 1:
		return rule.Name
	case 2:
		return rule.Template
	default:
		return ""
	}
}

type application struct {
	mainWindow          *walk.MainWindow
	notifyIcon          *walk.NotifyIcon
	settingsDialog      *walk.Dialog
	editorDialog        *walk.Dialog
	refreshSettingsView func()
	icon                *walk.Icon
	settings            appSettings
	configPath          string
	logPath             string
	lastText            string
	clipboardVersion    uint64
	lastTrayClick       time.Time
	trayClickTimer      *time.Timer
	settingsUp          bool
	exiting             bool
}

func main() {
	logPath := resolveLogPath()
	defer func() {
		if recovered := recover(); recovered != nil {
			logLine(logPath, fmt.Sprintf("未处理异常: %v\r\n%s", recovered, debug.Stack()))
			walk.MsgBox(nil, appTitle, "程序发生未处理异常，详情已写入：\r\n\r\n"+logPath, walk.MsgBoxIconError)
		}
	}()
	logLine(logPath, "程序启动（原生静态版）。")

	mutexName, _ := syscall.UTF16PtrFromString("BilibiliUrl.Native.SingleInstance.7C9E9C32")
	mutex, _, mutexErr := procCreateMutexW.Call(0, 1, uintptr(unsafe.Pointer(mutexName)))
	if mutex == 0 {
		showStartupError(logPath, fmt.Errorf("创建单实例锁失败: %v", mutexErr))
		return
	}
	defer procCloseHandle.Call(mutex)
	if errors.Is(mutexErr, errorAlreadyExists) {
		walk.MsgBox(nil, appTitle, "程序已经在运行，请检查任务栏右侧的隐藏图标。", walk.MsgBoxIconInformation)
		return
	}

	executable, err := os.Executable()
	if err != nil {
		showStartupError(logPath, err)
		return
	}
	configPath := filepath.Join(filepath.Dir(executable), configFileName)
	settings, err := loadOrCreateSettings(configPath)
	if err != nil {
		showStartupError(logPath, err)
		return
	}

	app := &application{settings: settings, configPath: configPath, logPath: logPath}
	if err := app.run(); err != nil {
		logLine(logPath, "启动失败: "+err.Error())
		walk.MsgBox(nil, appTitle, "程序启动失败：\r\n\r\n"+err.Error()+"\r\n\r\n日志："+logPath, walk.MsgBoxIconError)
	}
}

func (app *application) run() error {
	mw, err := walk.NewMainWindow()
	if err != nil {
		return err
	}
	app.mainWindow = mw
	mw.SetTitle(appTitle)
	mw.SetVisible(false)

	icon, err := walk.NewIconFromResourceId(2)
	if err != nil {
		return fmt.Errorf("读取内嵌图标失败: %w", err)
	}
	app.icon = icon
	mw.SetIcon(icon)

	ni, err := walk.NewNotifyIcon(mw)
	if err != nil {
		return fmt.Errorf("创建托盘图标失败: %w", err)
	}
	app.notifyIcon = ni
	ni.SetIcon(icon)
	ni.MouseUp().Attach(func(_, _ int, button walk.MouseButton) {
		if button == walk.LeftButton {
			now := time.Now()
			if !app.lastTrayClick.IsZero() && now.Sub(app.lastTrayClick) <= 500*time.Millisecond {
				if app.trayClickTimer != nil {
					app.trayClickTimer.Stop()
					app.trayClickTimer = nil
				}
				app.lastTrayClick = time.Time{}
				app.showSettings()
				return
			}

			app.lastTrayClick = now
			clickTime := now
			app.trayClickTimer = time.AfterFunc(500*time.Millisecond, func() {
				app.mainWindow.Synchronize(func() {
					if !app.lastTrayClick.Equal(clickTime) {
						return
					}
					app.lastTrayClick = time.Time{}
					app.trayClickTimer = nil
					app.showTrayMenu()
				})
			})
		}
	})
	app.rebuildTrayMenu()
	if err := ni.SetVisible(true); err != nil {
		return err
	}
	defer ni.Dispose()
	defer icon.Dispose()
	ni.ShowInfo(appTitle, "单击托盘图标可打开菜单，双击可打开设置。")

	stopPolling := make(chan struct{})
	go func() {
		ticker := time.NewTicker(750 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				app.mainWindow.Synchronize(app.pollClipboard)
			case <-stopPolling:
				return
			}
		}
	}()

	logLine(app.logPath, "托盘程序初始化完成。")
	mw.Run()
	close(stopPolling)
	return nil
}

func (app *application) showTrayMenu() {
	if app.exiting || app.notifyIcon == nil || app.mainWindow == nil {
		return
	}
	// NotifyIcon uses WM_APP as its callback message. Sending WM_CONTEXTMENU
	// through that callback invokes the same menu path as a right click.
	win.SendMessage(app.mainWindow.Handle(), win.WM_APP, 0, win.WM_CONTEXTMENU)
}

func (app *application) pollClipboard() {
	status := "监测中"
	if !app.settings.Enabled {
		status = "已暂停"
	} else if app.settings.OnlyWhenVRChatRunning {
		if !processRunning("VRChat.exe") {
			status = "等待 VRChat"
			app.notifyIcon.SetToolTip(appTitle + " - " + status)
			return
		}
		status = "VRChat 运行中"
	}
	app.notifyIcon.SetToolTip(appTitle + " - " + status)
	if !app.settings.Enabled {
		return
	}

	text, err := walk.Clipboard().Text()
	if err != nil || text == "" || text == app.lastText {
		return
	}
	app.lastText = text
	app.clipboardVersion++
	version := app.clipboardVersion
	rule := app.activeRule()
	converted, ok := convertBilibiliText(text, rule)
	if ok {
		app.replaceClipboard(text, converted, rule)
		return
	}
	shortURL, ok := extractB23Link(text)
	if !ok {
		return
	}
	go func() {
		resolved, err := resolveB23Link(shortURL)
		if err != nil {
			logLine(app.logPath, "解析 B站短链接失败: "+err.Error())
			return
		}
		converted, ok := convertLink(resolved, rule)
		if !ok {
			logLine(app.logPath, "B站短链接跳转结果不是可识别的视频链接。")
			return
		}
		app.mainWindow.Synchronize(func() {
			if app.clipboardVersion != version {
				return
			}
			app.replaceClipboard(text, converted, rule)
		})
	}()
}

func (app *application) replaceClipboard(original, converted string, rule conversionRule) {
	if app.exiting || converted == original {
		return
	}
	current, err := walk.Clipboard().Text()
	if err != nil || current != original {
		return
	}
	if err := walk.Clipboard().SetText(converted); err != nil {
		logLine(app.logPath, "写入剪贴板失败: "+err.Error())
		return
	}
	app.lastText = converted
	logLine(app.logPath, "已使用规则“"+rule.Name+"”转换剪贴板链接。")
}

func (app *application) activeRule() conversionRule {
	for _, rule := range app.settings.Rules {
		if strings.EqualFold(rule.ID, app.settings.ActiveRuleID) {
			return rule
		}
	}
	return app.settings.Rules[0]
}

func (app *application) applySettings(candidate appSettings) bool {
	if err := validateSettings(&candidate); err != nil {
		walk.MsgBox(app.mainWindow, appTitle, err.Error(), walk.MsgBoxIconWarning)
		return false
	}
	if err := saveSettings(app.configPath, candidate); err != nil {
		logLine(app.logPath, "保存配置失败: "+err.Error())
		walk.MsgBox(app.mainWindow, appTitle, "保存配置失败：\r\n\r\n"+err.Error(), walk.MsgBoxIconError)
		return false
	}
	app.settings = candidate
	if app.refreshSettingsView != nil {
		app.refreshSettingsView()
	}
	app.mainWindow.Synchronize(app.rebuildTrayMenu)
	return true
}

func (app *application) rebuildTrayMenu() {
	actions := app.notifyIcon.ContextMenu().Actions()
	actions.Clear()

	enabled := walk.NewAction()
	enabled.SetText(map[bool]string{true: "监测已开启", false: "监测已暂停"}[app.settings.Enabled])
	enabled.SetCheckable(true)
	enabled.SetChecked(app.settings.Enabled)
	enabled.Triggered().Attach(func() {
		candidate := cloneSettings(app.settings)
		candidate.Enabled = !candidate.Enabled
		app.applySettings(candidate)
	})
	actions.Add(enabled)

	onlyWhenVRChat := walk.NewAction()
	onlyWhenVRChat.SetText("仅在 VRChat 运行时转换")
	onlyWhenVRChat.SetCheckable(true)
	onlyWhenVRChat.SetChecked(app.settings.OnlyWhenVRChatRunning)
	onlyWhenVRChat.Triggered().Attach(func() {
		candidate := cloneSettings(app.settings)
		candidate.OnlyWhenVRChatRunning = !candidate.OnlyWhenVRChatRunning
		app.applySettings(candidate)
	})
	actions.Add(onlyWhenVRChat)
	actions.Add(walk.NewSeparatorAction())

	for _, configuredRule := range app.settings.Rules {
		rule := configuredRule
		action := walk.NewAction()
		action.SetText(rule.Name)
		action.SetCheckable(true)
		action.SetChecked(strings.EqualFold(rule.ID, app.settings.ActiveRuleID))
		action.Triggered().Attach(func() {
			candidate := cloneSettings(app.settings)
			candidate.ActiveRuleID = rule.ID
			app.applySettings(candidate)
		})
		actions.Add(action)
	}
	actions.Add(walk.NewSeparatorAction())

	settingsAction := walk.NewAction()
	settingsAction.SetText("设置...")
	settingsAction.Triggered().Attach(app.showSettings)
	actions.Add(settingsAction)

	exitAction := walk.NewAction()
	exitAction.SetText("退出")
	exitAction.Triggered().Attach(app.exit)
	actions.Add(exitAction)
}

func (app *application) exit() {
	if app.exiting {
		return
	}
	app.exiting = true
	if app.notifyIcon != nil {
		_ = app.notifyIcon.SetVisible(false)
	}
	logLine(app.logPath, "收到退出请求，托盘图标已移除，正在终止进程。")
	os.Exit(0)
}

func (app *application) showSettingsLegacy() {
	if app.settingsUp {
		return
	}
	app.settingsUp = true
	defer func() { app.settingsUp = false }()

	working := cloneSettings(app.settings)
	var dialog *walk.Dialog
	var ruleList *walk.ListBox
	var nameEdit, templateEdit *walk.LineEdit
	var enabledBox *walk.CheckBox
	loading := false

	labels := func() []string {
		items := make([]string, len(working.Rules))
		for i, rule := range working.Rules {
			prefix := ""
			if strings.EqualFold(rule.ID, working.ActiveRuleID) {
				prefix = "[当前] "
			}
			items[i] = prefix + rule.Name
		}
		return items
	}
	refreshList := func(index int) {
		loading = true
		ruleList.SetModel(labels())
		if index >= 0 && index < len(working.Rules) {
			ruleList.SetCurrentIndex(index)
		}
		loading = false
	}
	loadSelection := func() {
		if loading || ruleList == nil {
			return
		}
		i := ruleList.CurrentIndex()
		if i < 0 || i >= len(working.Rules) {
			return
		}
		loading = true
		nameEdit.SetText(working.Rules[i].Name)
		templateEdit.SetText(working.Rules[i].Template)
		loading = false
	}
	applyEdits := func() {
		if loading || ruleList == nil {
			return
		}
		i := ruleList.CurrentIndex()
		if i < 0 || i >= len(working.Rules) {
			return
		}
		candidate := cloneSettings(working)
		candidate.Rules[i].Name = strings.TrimSpace(nameEdit.Text())
		candidate.Rules[i].Template = strings.TrimSpace(templateEdit.Text())
		if app.applySettings(candidate) {
			working = candidate
			refreshList(i)
		} else {
			loadSelection()
		}
	}

	err := (Dialog{
		AssignTo: &dialog,
		Title:    appTitle + "设置",
		Icon:     app.icon,
		Size:     Size{Width: 820, Height: 430},
		MinSize:  Size{Width: 700, Height: 380},
		Layout:   VBox{MarginsZero: false},
		Children: []Widget{
			CheckBox{
				AssignTo:  &enabledBox,
				Alignment: AlignHNearVCenter,
				Text:      "启用监测（仅在 VRChat 运行时转换）",
				Checked:   working.Enabled,
				OnCheckedChanged: func() {
					if loading {
						return
					}
					candidate := cloneSettings(working)
					candidate.Enabled = enabledBox.Checked()
					if app.applySettings(candidate) {
						working = candidate
					} else {
						loading = true
						enabledBox.SetChecked(working.Enabled)
						loading = false
					}
				},
			},
			Label{Text: "模板支持 %url（标准 B站链接）和 %id（BV号）；编辑完成并离开输入框时立即生效。"},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					ListBox{
						AssignTo:              &ruleList,
						Model:                 labels(),
						MinSize:               Size{Width: 230, Height: 260},
						OnCurrentIndexChanged: loadSelection,
					},
					Composite{
						Layout: VBox{},
						Children: []Widget{
							Label{Text: "名称"},
							LineEdit{AssignTo: &nameEdit, OnEditingFinished: applyEdits},
							Label{Text: "转换模板"},
							LineEdit{AssignTo: &templateEdit, OnEditingFinished: applyEdits},
							VSpacer{},
						},
					},
				},
			},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					PushButton{Text: "添加", OnClicked: func() {
						candidate := cloneSettings(working)
						candidate.Rules = append(candidate.Rules, conversionRule{
							ID: fmt.Sprintf("rule-%d", time.Now().UnixNano()), Name: "新转换方式", Template: "%url",
						})
						if app.applySettings(candidate) {
							working = candidate
							refreshList(len(working.Rules) - 1)
							loadSelection()
						}
					}},
					PushButton{Text: "删除", OnClicked: func() {
						i := ruleList.CurrentIndex()
						if i < 0 || len(working.Rules) <= 1 {
							walk.MsgBox(dialog, appTitle, "至少需要保留一种转换方式。", walk.MsgBoxIconInformation)
							return
						}
						candidate := cloneSettings(working)
						removedID := candidate.Rules[i].ID
						candidate.Rules = append(candidate.Rules[:i], candidate.Rules[i+1:]...)
						if strings.EqualFold(candidate.ActiveRuleID, removedID) {
							candidate.ActiveRuleID = candidate.Rules[0].ID
						}
						if app.applySettings(candidate) {
							working = candidate
							if i >= len(working.Rules) {
								i = len(working.Rules) - 1
							}
							refreshList(i)
							loadSelection()
						}
					}},
					PushButton{Text: "设为当前", OnClicked: func() {
						i := ruleList.CurrentIndex()
						if i < 0 {
							return
						}
						candidate := cloneSettings(working)
						candidate.ActiveRuleID = candidate.Rules[i].ID
						if app.applySettings(candidate) {
							working = candidate
							refreshList(i)
						}
					}},
					HSpacer{},
				},
			},
			Label{Text: "配置文件：" + app.configPath},
		},
	}).Create(app.mainWindow)
	if err != nil {
		walk.MsgBox(app.mainWindow, appTitle, "打开设置失败：\r\n\r\n"+err.Error(), walk.MsgBoxIconError)
		return
	}
	ruleList.SetCurrentIndex(0)
	loadSelection()
	dialog.Run()
}

func (app *application) showSettings() {
	if app.exiting {
		return
	}
	if app.settingsUp {
		app.bringSettingsToFront()
		return
	}
	app.settingsUp = true
	logLine(app.logPath, "打开设置窗口。")
	defer func() {
		app.refreshSettingsView = nil
		app.settingsDialog = nil
		app.settingsUp = false
		logLine(app.logPath, "设置窗口已正常关闭。")
	}()

	working := cloneSettings(app.settings)
	model := &ruleTableModel{settings: &working}
	var dialog *walk.Dialog
	var table *walk.TableView
	var rowContextMenu *walk.Menu
	var enabledBox, onlyWhenVRChatBox *walk.CheckBox
	loading := false
	activatingWithEnter := false

	refreshTable := func(index int) {
		model.settings = &working
		model.PublishRowsReset()
		if len(working.Rules) > 0 {
			model.PublishRowsChanged(0, len(working.Rules)-1)
		}
		if table != nil {
			if index >= 0 && index < len(working.Rules) {
				table.SetCurrentIndex(index)
			}
			_ = table.Invalidate()
		}
	}
	refreshFromApp := func() {
		if table == nil || enabledBox == nil || onlyWhenVRChatBox == nil {
			return
		}
		selectedID := ""
		index := table.CurrentIndex()
		if index >= 0 && index < len(working.Rules) {
			selectedID = working.Rules[index].ID
		}

		working = cloneSettings(app.settings)
		index = 0
		for i, rule := range working.Rules {
			if strings.EqualFold(rule.ID, selectedID) {
				index = i
				break
			}
		}

		loading = true
		enabledBox.SetChecked(working.Enabled)
		onlyWhenVRChatBox.SetChecked(working.OnlyWhenVRChatRunning)
		loading = false
		refreshTable(index)
	}

	editSelected := func() {
		if table == nil {
			return
		}
		index := table.CurrentIndex()
		if index < 0 || index >= len(working.Rules) {
			return
		}
		app.editRule(dialog, &working, index, func() { refreshTable(index) })
	}
	setSelectedCurrent := func() {
		if table == nil {
			return
		}
		index := table.CurrentIndex()
		if index < 0 || index >= len(working.Rules) {
			return
		}
		candidate := cloneSettings(working)
		candidate.ActiveRuleID = candidate.Rules[index].ID
		if app.applySettings(candidate) {
			working = candidate
			refreshTable(index)
		}
	}
	deleteSelected := func() {
		if table == nil {
			return
		}
		index := table.CurrentIndex()
		if index < 0 || index >= len(working.Rules) {
			return
		}
		if len(working.Rules) <= 1 {
			walk.MsgBox(dialog, appTitle, "至少需要保留一种转换方式。", walk.MsgBoxIconInformation)
			return
		}
		candidate := cloneSettings(working)
		removedID := candidate.Rules[index].ID
		candidate.Rules = append(candidate.Rules[:index], candidate.Rules[index+1:]...)
		if strings.EqualFold(candidate.ActiveRuleID, removedID) {
			candidate.ActiveRuleID = candidate.Rules[0].ID
		}
		if app.applySettings(candidate) {
			working = candidate
			if index >= len(working.Rules) {
				index = len(working.Rules) - 1
			}
			refreshTable(index)
		}
	}
	testSelected := func() {
		if table == nil {
			return
		}
		index := table.CurrentIndex()
		if index < 0 || index >= len(working.Rules) {
			return
		}
		app.testRule(dialog, working.Rules[index])
	}

	err := (Dialog{
		AssignTo: &dialog,
		Title:    appTitle + "设置",
		Icon:     app.icon,
		Font:     Font{Family: "Microsoft YaHei UI", PointSize: 9},
		Size:     Size{Width: 900, Height: 500},
		MinSize:  Size{Width: 760, Height: 420},
		Layout:   VBox{Margins: Margins{Left: 14, Top: 14, Right: 14, Bottom: 12}, Spacing: 10},
		Children: []Widget{
			TextLabel{
				Text:          "本程序在后台监测剪贴板，将B站视频链接（含短链接）自动转换为VRChat播放器可直接播放的链接。转换结果会自动写回剪贴板，便于直接粘贴到 VRChat 播放器中。该设置页面可编辑转换模版。",
				MinSize:       Size{Width: 100},
				TextAlignment: AlignHNearVNear,
				TextColor:     walk.RGB(80, 80, 80),
			},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 24},
				Children: []Widget{
					CheckBox{
						AssignTo:  &enabledBox,
						Alignment: AlignHNearVCenter,
						Text:      "启用监测",
						Checked:   working.Enabled,
						OnCheckedChanged: func() {
							if loading {
								return
							}
							candidate := cloneSettings(working)
							candidate.Enabled = enabledBox.Checked()
							if app.applySettings(candidate) {
								working = candidate
								model.settings = &working
							} else {
								loading = true
								enabledBox.SetChecked(working.Enabled)
								loading = false
							}
						},
					},
					CheckBox{
						AssignTo:  &onlyWhenVRChatBox,
						Alignment: AlignHNearVCenter,
						Text:      "仅在 VRChat 运行时转换",
						Checked:   working.OnlyWhenVRChatRunning,
						OnCheckedChanged: func() {
							if loading {
								return
							}
							candidate := cloneSettings(working)
							candidate.OnlyWhenVRChatRunning = onlyWhenVRChatBox.Checked()
							if app.applySettings(candidate) {
								working = candidate
								model.settings = &working
							} else {
								loading = true
								onlyWhenVRChatBox.SetChecked(working.OnlyWhenVRChatRunning)
								loading = false
							}
						},
					},
					HSpacer{},
				},
			},
			Label{Text: "模板可使用 %url（标准 B站链接）和 %id（BV号）。按 Enter 设为当前，双击可编辑。"},
			TableView{
				AssignTo: &table,
				Model:    model,
				ContextMenuItems: []MenuItem{
					Action{Text: "设为当前", OnTriggered: setSelectedCurrent},
					Action{Text: "编辑", OnTriggered: editSelected},
					Action{Text: "删除", OnTriggered: deleteSelected},
					Action{Text: "测试", OnTriggered: testSelected},
				},
				ColumnsOrderable:            false,
				ColumnsSizable:              true,
				CustomRowHeight:             31,
				LastColumnStretched:         true,
				MultiSelection:              false,
				NotSortableByHeaderClick:    true,
				SelectionHiddenWithoutFocus: false,
				OnMouseDown: func(x, y int, button walk.MouseButton) {
					if button != walk.RightButton {
						return
					}
					index := table.IndexAt(x, y)
					if index >= 0 && index < len(working.Rules) {
						table.SetCurrentIndex(index)
					}
				},
				OnMouseUp: func(x, y int, button walk.MouseButton) {
					if button != walk.RightButton {
						return
					}
					index := table.IndexAt(x, y)
					if index < 0 || index >= len(working.Rules) {
						table.SetContextMenu(nil)
						return
					}
					table.SetCurrentIndex(index)
					table.SetContextMenu(rowContextMenu)
				},
				OnKeyDown: func(key walk.Key) {
					if key == walk.KeyReturn {
						activatingWithEnter = true
						setSelectedCurrent()
					}
				},
				OnKeyUp: func(key walk.Key) {
					if key == walk.KeyReturn {
						activatingWithEnter = false
					}
				},
				OnItemActivated: func() {
					if !activatingWithEnter {
						editSelected()
					}
				},
				Columns: []TableViewColumn{
					{Title: "状态", Width: 72},
					{Title: "名称", Width: 180},
					{Title: "转换模板", Width: 560},
				},
			},
			Label{Text: "配置文件：" + app.configPath, TextColor: walk.RGB(105, 105, 105)},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 8},
				Children: []Widget{
					PushButton{Text: "添加", OnClicked: func() {
						candidate := cloneSettings(working)
						candidate.Rules = append(candidate.Rules, conversionRule{
							ID: fmt.Sprintf("rule-%d", time.Now().UnixNano()), Name: "新转换方式", Template: "%url",
						})
						if app.applySettings(candidate) {
							working = candidate
							index := len(working.Rules) - 1
							refreshTable(index)
							app.editRule(dialog, &working, index, func() { refreshTable(index) })
						}
					}},
					PushButton{Text: "编辑", OnClicked: editSelected},
					PushButton{Text: "删除", OnClicked: deleteSelected},
					PushButton{Text: "测试", OnClicked: testSelected},
					HSpacer{},
					PushButton{Text: "设为当前", OnClicked: setSelectedCurrent},
				},
			},
		},
	}).Create(nil)
	if err != nil {
		walk.MsgBox(app.mainWindow, appTitle, "打开设置失败：\r\n\r\n"+err.Error(), walk.MsgBoxIconError)
		return
	}
	app.settingsDialog = dialog
	app.refreshSettingsView = refreshFromApp
	rowContextMenu = table.ContextMenu()
	table.SetGridlines(true)
	table.SetCurrentIndex(0)
	dialog.Starting().Attach(func() {
		if err := centerDialogOnScreen(dialog); err != nil {
			logLine(app.logPath, "设置窗口居中失败: "+err.Error())
		}
	})
	dialog.Run()
}

func (app *application) bringSettingsToFront() {
	dialog := app.settingsDialog
	if dialog == nil || dialog.Handle() == 0 {
		return
	}
	handle := dialog.Handle()
	if win.IsIconic(handle) {
		win.ShowWindow(handle, win.SW_RESTORE)
	} else if !win.IsWindowVisible(handle) {
		win.ShowWindow(handle, win.SW_SHOW)
	}
	_ = dialog.BringToTop()
	win.SetForegroundWindow(handle)
	win.SetActiveWindow(handle)
}

func centerDialogOnScreen(dialog *walk.Dialog) error {
	var monitorInfo win.MONITORINFO
	monitorInfo.CbSize = uint32(unsafe.Sizeof(monitorInfo))
	monitor := win.MonitorFromWindow(dialog.Handle(), win.MONITOR_DEFAULTTOPRIMARY)
	if monitor == 0 || !win.GetMonitorInfo(monitor, &monitorInfo) {
		return errors.New("无法获取显示器工作区")
	}

	bounds := dialog.BoundsPixels()
	workWidth := int(monitorInfo.RcWork.Right - monitorInfo.RcWork.Left)
	workHeight := int(monitorInfo.RcWork.Bottom - monitorInfo.RcWork.Top)
	x := int(monitorInfo.RcWork.Left)
	y := int(monitorInfo.RcWork.Top)
	if bounds.Width < workWidth {
		x += (workWidth - bounds.Width) / 2
	}
	if bounds.Height < workHeight {
		y += (workHeight - bounds.Height) / 2
	}
	return dialog.SetBoundsPixels(walk.Rectangle{X: x, Y: y, Width: bounds.Width, Height: bounds.Height})
}

func (app *application) testRule(owner walk.Form, rule conversionRule) {
	var dialog *walk.Dialog
	var inputEdit, outputEdit *walk.LineEdit
	var statusLabel *walk.Label
	var closeButton *walk.PushButton
	var requestVersion uint64

	updateResult := func() {
		if inputEdit == nil || outputEdit == nil || statusLabel == nil {
			return
		}
		requestVersion++
		version := requestVersion
		input := strings.TrimSpace(inputEdit.Text())
		if input == "" {
			outputEdit.SetText("")
			statusLabel.SetText("请输入需要测试的 B 站视频链接。")
			return
		}
		output, ok := convertBilibiliText(input, rule)
		if ok {
			outputEdit.SetText(output)
			statusLabel.SetText("解析成功")
			return
		}
		shortURL, ok := extractB23Link(input)
		if !ok {
			outputEdit.SetText("")
			statusLabel.SetText("无法识别该链接，请检查 B站域名、短链接和 BV 号。")
			return
		}
		outputEdit.SetText("")
		statusLabel.SetText("正在访问 B站短链接...")
		go func() {
			resolved, resolveErr := resolveB23Link(shortURL)
			converted := ""
			convertedOK := false
			if resolveErr == nil {
				converted, convertedOK = convertLink(resolved, rule)
			}
			app.mainWindow.Synchronize(func() {
				if dialog == nil || dialog.Handle() == 0 || requestVersion != version {
					return
				}
				if resolveErr != nil {
					outputEdit.SetText("")
					statusLabel.SetText("短链接解析失败：" + resolveErr.Error())
					return
				}
				if !convertedOK {
					outputEdit.SetText("")
					statusLabel.SetText("短链接跳转结果不是可识别的 B站视频链接。")
					return
				}
				outputEdit.SetText(converted)
				statusLabel.SetText("短链接解析成功")
			})
		}()
	}

	err := (Dialog{
		AssignTo:     &dialog,
		Title:        "测试转换方式",
		Icon:         app.icon,
		Font:         Font{Family: "Microsoft YaHei UI", PointSize: 9},
		Size:         Size{Width: 760, Height: 250},
		MinSize:      Size{Width: 620, Height: 230},
		Layout:       VBox{Margins: Margins{Left: 14, Top: 14, Right: 14, Bottom: 12}, Spacing: 8},
		CancelButton: &closeButton,
		Children: []Widget{
			Label{Text: "转换方式：" + rule.Name},
			Label{Text: "输入 URL 或分享文本"},
			LineEdit{
				AssignTo:      &inputEdit,
				CueBanner:     "例如：https://www.bilibili.com/video/BV1Xv8W6hEwZ 或 https://b23.tv/xxxxxxx",
				OnTextChanged: updateResult,
			},
			Label{Text: "解析结果"},
			LineEdit{AssignTo: &outputEdit, ReadOnly: true},
			Label{AssignTo: &statusLabel, Text: "请输入需要测试的 B 站视频链接。", TextColor: walk.RGB(105, 105, 105)},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					HSpacer{},
					PushButton{AssignTo: &closeButton, Text: "关闭", OnClicked: func() { dialog.Cancel() }},
				},
			},
		},
	}).Create(owner)
	if err != nil {
		walk.MsgBox(owner, appTitle, "打开测试窗口失败：\r\n\r\n"+err.Error(), walk.MsgBoxIconError)
		return
	}
	inputEdit.SetFocus()
	dialog.Run()
}

func (app *application) editRule(owner walk.Form, working *appSettings, index int, refreshed func()) {
	if index < 0 || index >= len(working.Rules) {
		return
	}
	var editor *walk.Dialog
	var nameEdit, templateEdit *walk.LineEdit
	var applyButton, closeButton *walk.PushButton
	rule := working.Rules[index]

	err := (Dialog{
		AssignTo:      &editor,
		Title:         "编辑转换方式",
		Icon:          app.icon,
		Font:          Font{Family: "Microsoft YaHei UI", PointSize: 9},
		Size:          Size{Width: 690, Height: 215},
		MinSize:       Size{Width: 580, Height: 200},
		Layout:        VBox{Margins: Margins{Left: 14, Top: 14, Right: 14, Bottom: 12}, Spacing: 8},
		DefaultButton: &applyButton,
		CancelButton:  &closeButton,
		Children: []Widget{
			Composite{
				Layout: Grid{Columns: 2, Spacing: 8},
				Children: []Widget{
					Label{Text: "名称"},
					LineEdit{AssignTo: &nameEdit, Text: rule.Name},
					Label{Text: "转换模板"},
					LineEdit{AssignTo: &templateEdit, Text: rule.Template},
				},
			},
			HSpacer{},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 8},
				Children: []Widget{
					HSpacer{},
					PushButton{AssignTo: &applyButton, Text: "应用修改", OnClicked: func() {
						candidate := cloneSettings(*working)
						candidate.Rules[index].Name = strings.TrimSpace(nameEdit.Text())
						candidate.Rules[index].Template = strings.TrimSpace(templateEdit.Text())
						if app.applySettings(candidate) {
							*working = candidate
							refreshed()
							editor.Accept()
						}
					}},
					PushButton{AssignTo: &closeButton, Text: "关闭", OnClicked: func() { editor.Cancel() }},
				},
			},
		},
	}).Create(owner)
	if err != nil {
		walk.MsgBox(owner, appTitle, "打开编辑窗口失败：\r\n\r\n"+err.Error(), walk.MsgBoxIconError)
		return
	}
	app.editorDialog = editor
	defer func() { app.editorDialog = nil }()
	editor.Run()
}

func defaultSettings() appSettings {
	return appSettings{
		Enabled: true, OnlyWhenVRChatRunning: true, ActiveRuleID: "sevenbrothers",
		Rules: []conversionRule{
			{ID: "sevenbrothers", Name: "sevenbrothers.cn", Template: "http://ckapi.sevenbrothers.cn/bili/api?id=%id"},
			{ID: "91vrchat", Name: "91vrchat.com", Template: "https://biliplayer.91vrchat.com/player/?url=%url"},
			{ID: "kemono-cn", Name: "kemono.cn", Template: "https://kemono.cn/b/?url=%url"},
		},
	}
}

func loadOrCreateSettings(path string) (appSettings, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(strings.TrimSpace(string(data))) == 0) {
		settings := defaultSettings()
		return settings, saveSettings(path, settings)
	}
	if err != nil {
		return appSettings{}, err
	}
	var settings appSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return appSettings{}, fmt.Errorf("配置文件格式错误: %w", err)
	}
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return appSettings{}, fmt.Errorf("配置文件格式错误: %w", err)
	}
	_, hasVRChatSetting := rawSettings["onlyWhenVRChatRunning"]
	if !hasVRChatSetting {
		settings.OnlyWhenVRChatRunning = true
	}
	if err := validateSettings(&settings); err != nil {
		return appSettings{}, err
	}
	if !hasVRChatSetting {
		if err := saveSettings(path, settings); err != nil {
			return appSettings{}, fmt.Errorf("升级配置文件失败: %w", err)
		}
	}
	return settings, nil
}

func saveSettings(path string, settings appSettings) error {
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o644); err != nil {
		return err
	}
	_ = os.Remove(path)
	return os.Rename(temporary, path)
}

func validateSettings(settings *appSettings) error {
	if len(settings.Rules) == 0 {
		return errors.New("至少需要一种转换方式。")
	}
	ids := map[string]bool{}
	activeFound := false
	for _, rule := range settings.Rules {
		if strings.TrimSpace(rule.ID) == "" || strings.TrimSpace(rule.Name) == "" {
			return errors.New("转换方式的 ID 和名称不能为空。")
		}
		lowerID := strings.ToLower(rule.ID)
		if ids[lowerID] {
			return errors.New("转换方式的 ID 不能重复。")
		}
		ids[lowerID] = true
		lowerTemplate := strings.ToLower(rule.Template)
		if !strings.Contains(lowerTemplate, "%url") && !strings.Contains(lowerTemplate, "%id") {
			return fmt.Errorf("“%s”的模板必须包含 %%url 或 %%id。", rule.Name)
		}
		if strings.EqualFold(rule.ID, settings.ActiveRuleID) {
			activeFound = true
		}
	}
	if !activeFound {
		settings.ActiveRuleID = settings.Rules[0].ID
	}
	return nil
}

func cloneSettings(source appSettings) appSettings {
	clone := source
	clone.Rules = append([]conversionRule(nil), source.Rules...)
	return clone
}

func convertBilibiliText(input string, rule conversionRule) (string, bool) {
	if converted, ok := convertLink(input, rule); ok {
		return converted, true
	}
	link, ok := extractEmbeddedBilibiliLink(input)
	if !ok {
		return "", false
	}
	return convertLink(link, rule)
}

func extractEmbeddedBilibiliLink(input string) (string, bool) {
	match := embeddedBilibiliLinkPattern.FindStringSubmatch(input)
	if len(match) != 2 {
		return "", false
	}
	return match[1], true
}

func extractB23Link(input string) (string, bool) {
	shortURL := b23LinkPattern.FindString(input)
	if shortURL == "" {
		return "", false
	}
	if !strings.Contains(shortURL, "://") {
		shortURL = "https://" + shortURL
	}
	parsed, err := url.Parse(shortURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || !isB23Host(parsed.Hostname()) {
		return "", false
	}
	return parsed.String(), true
}

func resolveB23Link(shortURL string) (string, error) {
	client := &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("跳转次数过多")
			}
			if request.URL.Scheme != "http" && request.URL.Scheme != "https" {
				return errors.New("短链接跳转到了不支持的协议")
			}
			host := request.URL.Hostname()
			if !isB23Host(host) && !isBilibiliHost(host) {
				return errors.New("短链接跳转到了非 B站域名")
			}
			return nil
		},
	}
	request, err := http.NewRequest(http.MethodGet, shortURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) BilibiliUrl/1.0")
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	resolved := response.Request.URL
	if resolved == nil || !isBilibiliHost(resolved.Hostname()) {
		return "", errors.New("短链接没有跳转到 B站视频页面")
	}
	return resolved.String(), nil
}

func isB23Host(host string) bool {
	host = strings.ToLower(host)
	return host == "b23.tv" || host == "www.b23.tv"
}

func isBilibiliHost(host string) bool {
	host = strings.ToLower(host)
	return host == "bilibili.com" || strings.HasSuffix(host, ".bilibili.com")
}

func convertLink(input string, rule conversionRule) (string, bool) {
	normalized := strings.TrimSpace(input)
	if !strings.Contains(normalized, "://") {
		normalized = "https://" + strings.TrimPrefix(normalized, "//")
	}
	parsed, err := url.Parse(normalized)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", false
	}
	if !isBilibiliHost(parsed.Hostname()) {
		return "", false
	}
	match := bvPathPattern.FindStringSubmatch(parsed.EscapedPath())
	if len(match) != 2 {
		return "", false
	}
	id := "BV" + match[1][2:]
	canonical := "https://www.bilibili.com/video/" + id
	output := replaceFold(rule.Template, "%url", canonical)
	output = replaceFold(output, "%id", id)
	return output, true
}

func replaceFold(source, token, value string) string {
	return regexp.MustCompile(`(?i)`+regexp.QuoteMeta(token)).ReplaceAllString(source, value)
}

func processRunning(executable string) bool {
	snapshot, _, _ := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if snapshot == 0 || snapshot == ^uintptr(0) {
		return false
	}
	defer procCloseHandle.Call(snapshot)
	entry := processEntry32{Size: uint32(unsafe.Sizeof(processEntry32{}))}
	ok, _, _ := procProcess32FirstW.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
	for ok != 0 {
		if strings.EqualFold(syscall.UTF16ToString(entry.ExeFile[:]), executable) {
			return true
		}
		ok, _, _ = procProcess32NextW.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
	}
	return false
}

func resolveLogPath() string {
	directory := filepath.Join(os.Getenv("LOCALAPPDATA"), "BilibiliUrl")
	if directory == "BilibiliUrl" || os.MkdirAll(directory, 0o755) != nil {
		return filepath.Join(os.TempDir(), "bilibili-url.log")
	}
	return filepath.Join(directory, "bilibili-url.log")
}

func logLine(path, message string) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "%s %s\r\n", time.Now().Format("2006-01-02 15:04:05.000"), message)
}

func showStartupError(logPath string, err error) {
	logLine(logPath, "启动失败: "+err.Error())
	walk.MsgBox(nil, appTitle, "程序启动失败：\r\n\r\n"+err.Error()+"\r\n\r\n日志："+logPath, walk.MsgBoxIconError)
}
