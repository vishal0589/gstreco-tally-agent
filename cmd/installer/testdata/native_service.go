package main

import (
	"fmt"
	"github.com/kardianos/service"
	"os"
)

var failure = "false"

type program struct{ done chan struct{} }

func (p *program) Start(service.Service) error {
	if failure == "true" {
		return fmt.Errorf("injected native service startup failure")
	}
	p.done = make(chan struct{})
	go func() { <-p.done }()
	return nil
}
func (p *program) Stop(service.Service) error { close(p.done); return nil }
func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("synthetic service 1.0.0")
		return
	}
	s, e := service.New(&program{}, &service.Config{Name: "gstreco-native-qa-only", DisplayName: "Disposable GST Reco native test"})
	if e != nil {
		panic(e)
	}
	if len(os.Args) > 2 && os.Args[1] == "service" {
		a := os.Args[2]
		if a == "status" {
			v, e := s.Status()
			if e != nil {
				fmt.Println("service not installed")
				os.Exit(1)
			}
			if v == service.StatusRunning {
				fmt.Println("service status: running")
			} else {
				fmt.Println("service status: stopped")
			}
			return
		}
		if a == "uninstall" {
			s.Stop()
		}
		if e := service.Control(s, a); e != nil {
			panic(e)
		}
		return
	}
	if e := s.Run(); e != nil {
		panic(e)
	}
}
