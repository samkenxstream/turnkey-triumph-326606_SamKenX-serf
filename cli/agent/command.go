package agent

import (
	"flag"
	"fmt"
	"github.com/hashicorp/serf/cli"
	"github.com/hashicorp/serf/rpc"
	"github.com/hashicorp/serf/serf"
	"net"
	"strings"
	"sync"
	"time"
)

// Command is a Command implementation that runs a Serf agent.
// The command will not end unless a shutdown message is sent on the
// ShutdownCh. If two messages are sent on the ShutdownCh it will forcibly
// exit.
type Command struct {
	ShutdownCh <-chan struct{}

	lock         sync.Mutex
	shuttingDown bool
}

func (c *Command) Help() string {
	helpText := `
Usage: serf agent [options]

  Starts the Serf agent and runs until an interrupt is received. The
  agent represents a single node in a cluster.

Options:

  -bind=0.0.0.0            Address to bind network listeners to
  -node=hostname           Name of this node. Must be unique in the cluster
  -rpc-addr=127.0.0.1:7373 Address to bind the RPC listener.
`
	return strings.TrimSpace(helpText)
}

func (c *Command) Run(args []string, ui cli.Ui) int {
	ui = &cli.PrefixedUi{
		OutputPrefix: "==> ",
		InfoPrefix:   "    ",
		ErrorPrefix:  "==> ",
		Ui:           ui,
	}

	var bindAddr string
	var nodeName string
	var rpcAddr string

	cmdFlags := flag.NewFlagSet("agent", flag.ContinueOnError)
	cmdFlags.Usage = func() { ui.Output(c.Help()) }
	cmdFlags.StringVar(&bindAddr, "bind", "0.0.0.0", "address to bind listeners to")
	cmdFlags.StringVar(&nodeName, "node", "", "node name")
	cmdFlags.StringVar(&rpcAddr, "rpc-addr", "127.0.0.1:7373",
		"address to bind RPC listener to")
	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	config := serf.DefaultConfig()
	config.MemberlistConfig.BindAddr = bindAddr
	config.NodeName = nodeName

	ui.Output("Starting Serf agent...")
	serf, err := serf.Create(config)
	if err != nil {
		ui.Error(fmt.Sprintf("Failed to initialize Serf: %s", err))
		return 1
	}
	defer serf.Shutdown()

	rpcL, err := net.Listen("tcp", rpcAddr)
	if err != nil {
		ui.Error(fmt.Sprintf("Failed to initialize RPC listener: %s", err))
		return 1
	}
	defer rpcL.Close()

	rpcServer, err := rpc.NewServer(serf, rpcL)
	if err != nil {
		ui.Error(fmt.Sprintf("Failed to initialize Serf: %s", err))
		return 1
	}
	go func() {
		if err := rpcServer.Run(); err != nil {
			c.lock.Lock()
			defer c.lock.Unlock()

			if !c.shuttingDown {
				panic(err)
			}
		}
	}()

	ui.Output("Serf agent running!")
	ui.Info(fmt.Sprintf("Node name: '%s'", config.NodeName))
	ui.Info(fmt.Sprintf("Bind addr: '%s'", config.MemberlistConfig.BindAddr))
	ui.Info(fmt.Sprintf(" RPC addr: '%s'", rpcAddr))

	graceful, forceful := c.startShutdownWatcher(serf, ui)
	select {
	case <-graceful:
	case <-forceful:
		// Forcefully shut down, return a bad exit status.
		return 1
	}

	return 0
}

func (c *Command) Synopsis() string {
	return "runs a Serf agent"
}

func (c *Command) forceShutdown(serf *serf.Serf, ui cli.Ui) {
	ui.Output("Forcefully shutting down agent...")
	if err := serf.Shutdown(); err != nil {
		ui.Error(fmt.Sprintf("Error: %s", err))
	}
}

func (c *Command) gracefulShutdown(serf *serf.Serf, ui cli.Ui, done chan<- struct{}) {
	ui.Output("Gracefully shutting down agent. " +
		"Interrupt again to forcefully shut down.")
	if err := serf.Leave(); err != nil {
		ui.Error(fmt.Sprintf("Error: %s", err))
		return
	}
	close(done)
}

func (c *Command) startShutdownWatcher(serf *serf.Serf, ui cli.Ui) (graceful <-chan struct{}, forceful <-chan struct{}) {
	g := make(chan struct{})
	f := make(chan struct{})
	graceful = g
	forceful = f

	go func() {
		<-c.ShutdownCh

		c.lock.Lock()
		c.shuttingDown = true
		c.lock.Unlock()

		go c.gracefulShutdown(serf, ui, g)

		select {
		case <-g:
			// Gracefully shut down properly
		case <-c.ShutdownCh:
			time.Sleep(50 * time.Millisecond)
			c.forceShutdown(serf, ui)
			close(f)
		}
	}()

	return
}