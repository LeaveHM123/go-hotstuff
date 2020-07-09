package main

import (
	"flag"
	"github.com/wjbbig/go-hotstuff/factory"
	"github.com/wjbbig/go-hotstuff/consensus"
	"github.com/wjbbig/go-hotstuff/logging"
	"github.com/wjbbig/go-hotstuff/proto"
	"google.golang.org/grpc"
	"net"
	"strings"
)

var (
	id          int
	networkType string
	logger      = logging.GetLogger()
	//sigChan     chan os.Signal
	//done        chan bool
)

func init() {
	//sigChan = make(chan os.Signal, 1)
	//signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	//done = make(chan bool)
	flag.IntVar(&id, "id", 0, "node id")
	flag.StringVar(&networkType, "type", "basic", "which type of network you want to create.  basic/chained/event-driven")
}

func main() {
	flag.Parse()
	if id <= 0 {
		flag.Usage()
		return
	}
	// create grpc server
	rpcServer := grpc.NewServer()

	hotStuffService := consensus.NewHotStuffService(factory.HotStuffFactory(networkType, id))
	proto.RegisterBasicHotStuffServer(rpcServer, hotStuffService)
	// get node port
	info := hotStuffService.GetImpl().GetSelfInfo()
	port := info.Address[strings.Index(info.Address, ":"):]
	logger.Infof("[HOTSTUFF] Server type: %v", networkType)
	logger.Infof("[HOTSTUFF] Server start at port%s", port)
	// listen the port
	listen, err := net.Listen("tcp", port)
	if err != nil {
		panic(err)
	}
	// close goroutine,db connection and delete db file safe when exiting
	//go func() {
	//	<-sigChan
	//	logger.Info("[HOTSTUFF] Shut down...")
	//	hotStuffService.GetImpl().SafeExit()
	//	done <- true
	//}()
	// start server
	rpcServer.Serve(listen)
	//<-done
}
