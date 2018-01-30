package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/jedisct1/dlog"
	"github.com/kardianos/service"
)

const (
	AppVersion     = "2.0.0beta11"
	ConfigFileName = "dnscrypt-proxy.toml"
)

type App struct {
	wg    sync.WaitGroup
	quit  chan struct{}
	proxy Proxy
}

func main() {
	dlog.Init("dnscrypt-proxy", dlog.SeverityNotice, "DAEMON")

	cdLocal()

	svcConfig := &service.Config{
		Name:        "dnscrypt-proxy",
		DisplayName: "DNSCrypt client proxy",
		Description: "Encrypted/authenticated DNS proxy",
	}
	svcFlag := flag.String("service", "", fmt.Sprintf("Control the system service: %q", service.ControlAction))
	app := &App{}
	svc, err := service.New(app, svcConfig)
	if err != nil {
		svc = nil
		dlog.Debug(err)
	}
	app.proxy = Proxy{}

	cdFileDir(ConfigFileName)
	if err := ConfigLoad(&app.proxy, svcFlag, ConfigFileName); err != nil {
		dlog.Fatal(err)
	}
	dlog.Noticef("Starting dnscrypt-proxy %s", AppVersion)

	if len(*svcFlag) != 0 {
		if err := service.Control(svc, *svcFlag); err != nil {
			dlog.Fatal(err)
		}
		if *svcFlag == "install" {
			dlog.Notice("Installed as a service. Use `-service start` to start")
		} else if *svcFlag == "uninstall" {
			dlog.Notice("Service uninstalled")
		} else if *svcFlag == "start" {
			dlog.Notice("Service started")
		} else if *svcFlag == "stop" {
			dlog.Notice("Service stopped")
		} else if *svcFlag == "restart" {
			dlog.Notice("Service restarted")
		}
		return
	}
	if svc != nil {
		if err = svc.Run(); err != nil {
			dlog.Fatal(err)
		}
	} else {
		app.Start(nil)
	}
}

func (app *App) Start(service service.Service) error {
	proxy := app.proxy
	proxy.cachedIPs.cache = make(map[string]string)
	if err := InitPluginsGlobals(&proxy.pluginsGlobals, &proxy); err != nil {
		dlog.Fatal(err)
	}
	if proxy.daemonize {
		Daemonize()
	}
	app.quit = make(chan struct{})
	app.wg.Add(1)
	if service != nil {
		go func() {
			app.AppMain(&proxy)
		}()
	} else {
		app.AppMain(&proxy)
	}
	return nil
}

func (app *App) AppMain(proxy *Proxy) {
	proxy.StartProxy()
	<-app.quit
	dlog.Notice("Quit signal received...")
	app.wg.Done()
}

func (app *App) Stop(service service.Service) error {
	dlog.Notice("Stopped.")
	return nil
}

func cdFileDir(fileName string) {
	os.Chdir(filepath.Dir(fileName))
}

func cdLocal() {
	exeFileName, err := os.Executable()
	if err != nil {
		dlog.Warnf("Unable to determine the executable directory: [%s] -- You will need to specify absolute paths in the configuration file", err)
		return
	}
	os.Chdir(filepath.Dir(exeFileName))
}
