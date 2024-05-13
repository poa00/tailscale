package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/cilium/ebpf/link"
)

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go bpf xdp.c -- -I headers

var (
	flagDevice = flag.String("device", "", "target device name")
)

func main() {
	flag.Parse()
	if len(*flagDevice) < 1 {
		log.Fatal("device flag is unset")
	}
	iface, err := net.InterfaceByName(*flagDevice)
	if err != nil {
		log.Panic(err)
	}

	objs := bpfObjects{}
	err = loadBpfObjects(&objs, nil)
	if err != nil {
		log.Panic(err)
	}
	defer objs.Close()

	l, err := link.AttachXDP(link.XDPOptions{
		Program:   objs.XdpProgFunc,
		Interface: iface.Index,
	})
	if err != nil {
		log.Panic(err)
	}
	defer l.Close()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
}
