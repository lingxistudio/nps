package main

import (
	"ehang.io/nps/client"
	"ehang.io/nps/lib/common"
	"ehang.io/nps/lib/config"
	"ehang.io/nps/lib/file"
	"ehang.io/nps/lib/install"
	"ehang.io/nps/lib/version"
	"flag"
	"fmt"
	"github.com/astaxie/beego/logs"
	"github.com/ccding/go-stun/stun"
	"github.com/kardianos/service"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

var (
	serverAddr     = flag.String("server", "", "Server addr (ip:port)")
	configPath     = flag.String("config", "", "Configuration file path")
	verifyKey      = flag.String("vkey", "", "Authentication key")
	logType        = flag.String("log", "stdout", "Log output mode（stdout|file）")
	connType       = flag.String("type", "tcp", "Connection type with the server（kcp|tcp|tls）")
	tlsVerify      = flag.Bool("tls_verify", false, "verify the server certificate when type=tls")
	proxyUrl       = flag.String("proxy", "", "proxy socks5 url(eg:socks5://111:222@127.0.0.1:9007)")
	logLevel       = flag.String("log_level", "7", "log level 0~7")
	registerTime   = flag.Int("time", 2, "register time long /h")
	localPort      = flag.Int("local_port", 2000, "p2p local port")
	password       = flag.String("password", "", "p2p password flag")
	target         = flag.String("target", "", "p2p target")
	localType      = flag.String("local_type", "p2p", "p2p target")
	logPath        = flag.String("log_path", "", "npc log path")
	debug          = flag.Bool("debug", true, "npc debug")
	pprofAddr      = flag.String("pprof", "", "PProf debug addr (ip:port)")
	stunAddr       = flag.String("stun_addr", "stun.stunprotocol.org:3478", "stun server address (eg:stun.stunprotocol.org:3478)")
	ver            = flag.Bool("version", false, "show current version")
	disconnectTime = flag.Int("disconnect_timeout", 60, "not receiving check packet times, until timeout will disconnect the client")
	apiAddr        = flag.String("api_addr", "", "local REST api listen addr (eg:127.0.0.1:8089), empty disables the api")
	apiKey         = flag.String("api_key", "", "REST api credential, empty disables the api")
	apiAllowIP     = flag.String("api_allow_ip", "", "REST api source ip whitelist, comma separated ip/cidr")
	apiReadOnly    = flag.Bool("api_read_only", false, "REST api read only mode")
)

func main() {
	flag.Parse()
	logs.Reset()
	logs.EnableFuncCallDepth(true)
	logs.SetLogFuncCallDepth(3)
	if *ver {
		common.PrintVersion()
		return
	}
	if *logPath == "" {
		*logPath = common.GetNpcLogPath()
	}
	if common.IsWindows() {
		*logPath = strings.Replace(*logPath, "\\", "\\\\", -1)
	}
	if *debug {
		logs.SetLogger(logs.AdapterConsole, `{"level":`+*logLevel+`,"color":true}`)
	} else {
		logs.SetLogger(logs.AdapterFile, `{"level":`+*logLevel+`,"filename":"`+*logPath+`","daily":false,"maxlines":100000,"color":true}`)
	}

	// init service
	options := make(service.KeyValue)
	svcConfig := &service.Config{
		Name:        "Npc",
		DisplayName: "nps内网穿透客户端",
		Description: "一款轻量级、功能强大的内网穿透代理服务器。支持tcp、udp流量转发，支持内网http代理、内网socks5代理，同时支持snappy压缩、站点保护、加密传输、多路复用、header修改等。支持web图形化管理，集成多用户模式。",
		Option:      options,
	}
	if !common.IsWindows() {
		svcConfig.Dependencies = []string{
			"Requires=network.target",
			"After=network-online.target syslog.target"}
		svcConfig.Option["SystemdScript"] = install.SystemdScript
		svcConfig.Option["SysvScript"] = install.SysvScript
	}
	for _, v := range os.Args[1:] {
		switch v {
		case "install", "start", "stop", "uninstall", "restart":
			continue
		}
		if !strings.Contains(v, "-service=") && !strings.Contains(v, "-debug=") {
			svcConfig.Arguments = append(svcConfig.Arguments, v)
		}
	}
	svcConfig.Arguments = append(svcConfig.Arguments, "-debug=false")
	prg := &npc{
		exit: make(chan struct{}),
	}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		logs.Error(err, "service function disabled")
		run()
		// run without service
		wg := sync.WaitGroup{}
		wg.Add(1)
		wg.Wait()
		return
	}
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "status":
			if len(os.Args) > 2 {
				path := strings.Replace(os.Args[2], "-config=", "", -1)
				client.GetTaskStatus(path)
			}
		case "register":
			flag.CommandLine.Parse(os.Args[2:])
			client.RegisterLocalIp(*serverAddr, *verifyKey, *connType, *proxyUrl, *registerTime)
		case "update":
			install.UpdateNpc()
			return
		case "nat":
			c := stun.NewClient()
			c.SetServerAddr(*stunAddr)
			nat, host, err := c.Discover()
			if err != nil || host == nil {
				logs.Error("get nat type error", err)
				return
			}
			fmt.Printf("nat type: %s \npublic address: %s\n", nat.String(), host.String())
			os.Exit(0)
		case "start", "stop", "restart":
			// support busyBox and sysV, for openWrt
			if service.Platform() == "unix-systemv" {
				logs.Info("unix-systemv service")
				cmd := exec.Command("/etc/init.d/"+svcConfig.Name, os.Args[1])
				err := cmd.Run()
				if err != nil {
					logs.Error(err)
				}
				return
			}
			err := service.Control(s, os.Args[1])
			if err != nil {
				logs.Error("Valid actions: %q\n%s", service.ControlAction, err.Error())
			}
			return
		case "install":
			service.Control(s, "stop")
			service.Control(s, "uninstall")
			install.InstallNpc()
			err := service.Control(s, os.Args[1])
			if err != nil {
				logs.Error("Valid actions: %q\n%s", service.ControlAction, err.Error())
			}
			if service.Platform() == "unix-systemv" {
				logs.Info("unix-systemv service")
				confPath := "/etc/init.d/" + svcConfig.Name
				os.Symlink(confPath, "/etc/rc.d/S90"+svcConfig.Name)
				os.Symlink(confPath, "/etc/rc.d/K02"+svcConfig.Name)
			}
			return
		case "uninstall":
			err := service.Control(s, os.Args[1])
			if err != nil {
				logs.Error("Valid actions: %q\n%s", service.ControlAction, err.Error())
			}
			if service.Platform() == "unix-systemv" {
				logs.Info("unix-systemv service")
				os.Remove("/etc/rc.d/S90" + svcConfig.Name)
				os.Remove("/etc/rc.d/K02" + svcConfig.Name)
			}
			return
		}
	}
	s.Run()
}

type npc struct {
	exit chan struct{}
}

func (p *npc) Start(s service.Service) error {
	go p.run()
	return nil
}
func (p *npc) Stop(s service.Service) error {
	close(p.exit)
	if service.Interactive() {
		os.Exit(0)
	}
	return nil
}

func (p *npc) run() error {
	defer func() {
		if err := recover(); err != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			logs.Warning("npc: panic serving %v: %v\n%s", err, string(buf))
		}
	}()
	run()
	select {
	case <-p.exit:
		logs.Warning("stop...")
	}
	return nil
}

func run() {
	common.InitPProfFromArg(*pprofAddr)
	// conn_type=tls 时是否校验服务端证书
	client.SetTlsVerify(*tlsVerify)
	//p2p or secret command
	if *password != "" {
		commonConfig := new(config.CommonConfig)
		commonConfig.Server = *serverAddr
		commonConfig.VKey = *verifyKey
		commonConfig.Tp = *connType
		localServer := new(config.LocalServer)
		localServer.Type = *localType
		localServer.Password = *password
		localServer.Target = *target
		localServer.Port = *localPort
		commonConfig.Client = new(file.Client)
		commonConfig.Client.Cnf = new(file.Config)
		go client.StartLocalServer(localServer, commonConfig)
		return
	}
	env := common.GetEnvMap()
	if *serverAddr == "" {
		*serverAddr, _ = env["NPC_SERVER_ADDR"]
	}
	if *verifyKey == "" {
		*verifyKey, _ = env["NPC_SERVER_VKEY"]
	}
	logs.Info("the version of client is %s, the core version of client is %s", version.VERSION, version.GetVersion())
	if *verifyKey != "" && *serverAddr != "" && *configPath == "" {
		// server 支持逗号 / 空格分隔的多个地址，每个地址独立连接、独立重连
		servers := config.SplitServerAddr(*serverAddr)
		if len(servers) > 1 {
			logs.Info("multi-server mode enabled, %d servers: %s", len(servers), strings.Join(servers, ", "))
		}
		m, err := client.NewCliManager(client.CliConfig{
			VKey:           *verifyKey,
			Tp:             *connType,
			ProxyUrl:       *proxyUrl,
			DisconnectTime: *disconnectTime,
			TlsVerify:      *tlsVerify,
		}, servers)
		if err != nil {
			logs.Error("invalid server address: %s", err.Error())
			return
		}
		startClientApi(m)
		go m.Start()
	} else {
		if *configPath == "" {
			*configPath = common.GetConfigPath()
		}
		m, err := client.NewFileManager(*configPath)
		if err != nil {
			logs.Error("%s", err.Error())
			return
		}
		logs.Info("loading configuration file %s successfully, servers: %s", *configPath, strings.Join(serverAddrs(m), ", "))
		startClientApi(m)
		go m.Start()
	}
}

// startClientApi 按「命令行参数 > 配置文件 [common] > 环境变量」的优先级启动本地管理 API。
// 没配地址或没配 key 都不会启动，避免无凭据的接口暴露在公网上。
func startClientApi(m *client.Manager) {
	addr := strings.TrimSpace(*apiAddr)
	key := strings.TrimSpace(*apiKey)
	allowIP := strings.TrimSpace(*apiAllowIP)
	readOnly := *apiReadOnly
	if cfg := m.FileConfig(); cfg != nil && cfg.CommonConfig != nil {
		if addr == "" {
			addr = strings.TrimSpace(cfg.CommonConfig.ApiAddr)
		}
		if key == "" {
			key = strings.TrimSpace(cfg.CommonConfig.ApiKey)
		}
		if allowIP == "" {
			allowIP = strings.TrimSpace(cfg.CommonConfig.ApiAllowIP)
		}
		if !readOnly {
			readOnly = cfg.CommonConfig.ApiReadOnly
		}
	}
	env := common.GetEnvMap()
	if addr == "" {
		addr = strings.TrimSpace(env["NPC_API_ADDR"])
	}
	if key == "" {
		key = strings.TrimSpace(env["NPC_API_KEY"])
	}
	if addr == "" {
		return
	}
	if key == "" {
		logs.Warn("api_addr is set but api_key is empty, the client api stays disabled")
		return
	}
	if _, err := client.StartApiServer(client.ApiOptions{
		Addr:     addr,
		Key:      key,
		AllowIP:  allowIP,
		ReadOnly: readOnly,
		Manager:  m,
	}); err != nil {
		logs.Error("start client api failed: %s", err.Error())
		return
	}
	logs.Notice("client api enabled on %s (read_only=%v)", addr, readOnly)
}

func serverAddrs(m *client.Manager) []string {
	servers := m.Servers()
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.Addr)
	}
	return out
}
