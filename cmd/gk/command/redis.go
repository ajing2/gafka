package command

import (
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/funkygao/Go-Redis"
	"github.com/funkygao/gafka/ctx"
	"github.com/funkygao/gafka/zk"
	"github.com/funkygao/go-metrics"
	"github.com/funkygao/gocli"
	"github.com/funkygao/golib/gofmt"
	log "github.com/funkygao/log4go"
	"github.com/funkygao/termui"
	"github.com/mattn/go-runewidth"
	"github.com/nsf/termbox-go"
	"github.com/pmylund/sortutil"
	"github.com/ryanuber/columnize"
)

type Redis struct {
	Ui  cli.Ui
	Cmd string

	mu           sync.Mutex
	topInfos     []redisTopInfo
	freezedPorts map[string]struct{}
	freezeN      int

	quit           chan struct{}
	rows           int
	topOrderAsc    bool
	topOrderColIdx int
	beep           int64
	topOrderCols   []string
	ipInNum        bool
	ports          map[string]struct{}
}

func (this *Redis) Run(args []string) (exitCode int) {
	var (
		zone        string
		add         string
		list        bool
		byHost      int
		del         string
		top         bool
		topInterval time.Duration
		ports       string
		ping        bool
	)
	cmdFlags := flag.NewFlagSet("redis", flag.ContinueOnError)
	cmdFlags.Usage = func() { this.Ui.Output(this.Help()) }
	cmdFlags.StringVar(&zone, "z", ctx.ZkDefaultZone(), "")
	cmdFlags.StringVar(&add, "add", "", "")
	cmdFlags.BoolVar(&list, "list", true, "")
	cmdFlags.IntVar(&byHost, "host", 0, "")
	cmdFlags.BoolVar(&top, "top", false, "")
	cmdFlags.DurationVar(&topInterval, "sleep", time.Second*7, "")
	cmdFlags.BoolVar(&ping, "ping", false, "")
	cmdFlags.BoolVar(&this.ipInNum, "n", false, "")
	cmdFlags.Int64Var(&this.beep, "beep", 0, "")
	cmdFlags.IntVar(&this.freezeN, "freeze", 20, "")
	cmdFlags.StringVar(&del, "del", "", "")
	cmdFlags.StringVar(&ports, "port", "", "")
	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	zkzone := zk.NewZkZone(zk.DefaultConfig(zone, ctx.ZoneZkAddrs(zone)))
	if top || ping {
		list = false
	}

	if add != "" {
		host, port, err := net.SplitHostPort(add)
		swallow(err)

		nport, err := strconv.Atoi(port)
		swallow(err)
		zkzone.AddRedis(host, nport)
	} else if del != "" {
		host, port, err := net.SplitHostPort(del)
		swallow(err)

		nport, err := strconv.Atoi(port)
		swallow(err)
		zkzone.DelRedis(host, nport)
	} else {
		if top {
			this.quit = make(chan struct{})
			this.topOrderAsc = false
			this.topOrderColIdx = 2 // ops by default
			this.topOrderCols = []string{"dbsize", "conns", "ops", "mem", "maxmem", "memp", "rx", "tx"}
			this.freezedPorts = make(map[string]struct{})
			this.ports = make(map[string]struct{})
			for _, p := range strings.Split(ports, ",") {
				tp := strings.TrimSpace(p)
				if tp != "" {
					this.ports[tp] = struct{}{}
				}

			}

			this.runTop(zkzone, topInterval)
		} else if ping {
			this.runPing(zkzone)
		} else if list {
			machineMap := make(map[string]struct{})
			machinePortMap := make(map[string][]string)
			var machines []string
			hostPorts := zkzone.AllRedis()
			sort.Strings(hostPorts)
			for _, hp := range hostPorts {
				host, port, _ := net.SplitHostPort(hp)
				ips, _ := net.LookupIP(host)
				ip := ips[0].String()
				if _, present := machineMap[ip]; !present {
					machineMap[ip] = struct{}{}
					machinePortMap[ip] = make([]string, 0)

					machines = append(machines, ip)
				}

				if byHost == 0 {
					this.Ui.Output(fmt.Sprintf("%35s %s", host, port))
				} else {
					machinePortMap[ip] = append(machinePortMap[ip], port)
				}

			}

			if byHost > 0 {
				sort.Strings(machines)
				for _, ip := range machines {
					sort.Strings(machinePortMap[ip])
					this.Ui.Info(fmt.Sprintf("%20s %2d ports", ip, len(machinePortMap[ip])))
					if byHost > 1 {
						this.Ui.Output(fmt.Sprintf("%+v", machinePortMap[ip]))
					}

				}
			}

			this.Ui.Output(fmt.Sprintf("Total instances:%d machines:%d", len(hostPorts), len(machines)))
		}
	}

	return
}

type redisTopInfo struct {
	host                                    string
	port                                    int
	dbsize, ops, rx, tx, conns, mem, maxmem int64
	memp                                    float64
	t0                                      time.Time
	latency                                 time.Duration
}

func (this *Redis) runTop(zkzone *zk.ZkZone, interval time.Duration) {
	termui.Init()
	this.rows = termui.TermHeight() - 2
	defer termui.Close()

	termbox.SetInputMode(termbox.InputEsc)
	eventChan := make(chan termbox.Event, 16)
	go this.handleEvents(eventChan)
	go func() {
		for {
			ev := termbox.PollEvent()
			eventChan <- ev
		}
	}()

	this.drawSplash()

	this.topInfos = make([]redisTopInfo, 0, 100)
	tick := time.NewTicker(interval)
	for {
		var wg sync.WaitGroup
		this.topInfos = this.topInfos[:0]
		freezedPorts := make(map[string]struct{})

		// clone freezedPorts to avoid concurrent map access
		this.mu.Lock()
		if len(this.freezedPorts) > 0 {
			for port, _ := range this.freezedPorts {
				freezedPorts[port] = struct{}{}
			}
		}
		this.mu.Unlock()

		for _, hostPort := range zkzone.AllRedis() {
			host, port, err := net.SplitHostPort(hostPort)
			if err != nil {
				log.Error("invalid redis instance: %s", hostPort)
				continue
			}

			if len(this.ports) > 0 {
				if _, present := this.ports[port]; !present {
					continue
				}
			}
			if len(freezedPorts) > 0 {
				if _, present := freezedPorts[port]; !present {
					continue
				}
			}

			nport, err := strconv.Atoi(port)
			if err != nil || nport < 0 {
				log.Error("invalid redis instance: %s", hostPort)
				continue
			}

			wg.Add(1)
			go this.updateRedisInfo(&wg, host, nport)
		}
		wg.Wait()

		this.render()

		select {
		case <-tick.C:

		case <-this.quit:
			return
		}
	}
}

func (this *Redis) handleEvents(eventChan chan termbox.Event) {
	for ev := range eventChan {
		switch ev.Type {
		case termbox.EventKey:
			switch ev.Key {
			case termbox.KeyEsc:
				close(this.quit)
				termbox.Close()
				os.Exit(0)
				return

			case termbox.KeyArrowUp:
				this.topOrderAsc = true
				this.render()

			case termbox.KeyArrowDown:
				this.topOrderAsc = false
				this.render()

			case termbox.KeyArrowLeft:
				this.topOrderColIdx--
				if this.topOrderColIdx < 0 {
					this.topOrderColIdx += len(this.topOrderCols)
				}
				this.render()

			case termbox.KeyArrowRight:
				this.topOrderColIdx++
				if this.topOrderColIdx >= len(this.topOrderCols) {
					this.topOrderColIdx -= len(this.topOrderCols)
				}
				this.render()
			}

			switch ev.Ch {
			case 'q':
				close(this.quit)
				termbox.Close()
				os.Exit(0)
				return

			case 'f':
				// freeze the topN
				this.mu.Lock()
				if len(this.topInfos) > this.freezeN {
					// topInfos already sorted by render()
					for _, info := range this.topInfos[:this.freezeN] {
						this.freezedPorts[strconv.Itoa(info.port)] = struct{}{}
					}

					this.topInfos = this.topInfos[:this.freezeN]
					this.render()
				}
				this.mu.Unlock()

			case 'F':
				// unfreeze
				this.mu.Lock()
				this.freezedPorts = make(map[string]struct{})
				this.mu.Unlock()
			}

		}
	}
}

func (this *Redis) drawSplash() {
	splashes := []string{"loading", "loading redis", "loading redis top..."}
	w, h := termbox.Size()
	x, y := w/2-len(splashes[2])/2, h/2+1
	for _, row := range splashes {
		termbox.Clear(termbox.ColorDefault, termbox.ColorDefault)
		for i, c := range row {
			termbox.SetCell(x+i, y, c, termbox.ColorGreen, termbox.ColorDefault)
		}
		termbox.Flush()
		time.Sleep(time.Millisecond * 1500)
	}
}

func (this *Redis) drawRow(row string, y int, fg, bg termbox.Attribute) {
	x := 0
	tuples := strings.SplitN(row, "|", 10)
	row = fmt.Sprintf("%25s %8s %15s %9s %9s %13s %13s %6s %13s %13s",
		tuples[0], tuples[1], tuples[2], tuples[3], tuples[4],
		tuples[5], tuples[6], tuples[7], tuples[8], tuples[9])
	for _, r := range row {
		termbox.SetCell(x, y, r, fg, bg)
		// wide string must be considered
		w := runewidth.RuneWidth(r)
		if w == 0 || (w == 2 && runewidth.IsAmbiguousWidth(r)) {
			w = 1
		}
		x += w
	}

}

func (this *Redis) selectedCol() string {
	return this.topOrderCols[this.topOrderColIdx]
}

func (this *Redis) render() {
	termbox.Clear(termbox.ColorDefault, termbox.ColorDefault)

	this.mu.Lock()
	defer this.mu.Unlock()

	if this.topOrderAsc {
		sortutil.AscByField(this.topInfos, this.topOrderCols[this.topOrderColIdx])
	} else {
		sortutil.DescByField(this.topInfos, this.topOrderCols[this.topOrderColIdx])
	}
	sortCols := make([]string, len(this.topOrderCols))
	copy(sortCols, this.topOrderCols)
	for i, col := range sortCols {
		if col == this.selectedCol() {
			if this.topOrderAsc {
				sortCols[i] += " >"
			} else {
				sortCols[i] += " <"
			}
		}
	}
	lines := []string{fmt.Sprintf("Host|Port|%s", strings.Join(sortCols, "|"))}

	var (
		sumDbsize, sumConns, sumOps, sumMem, sumRx, sumTx, sumMaxMem int64
	)
	for i := 0; i < len(this.topInfos); i++ {
		info := this.topInfos[i]
		sumDbsize += info.dbsize
		sumConns += info.conns
		sumOps += info.ops
		sumMem += info.mem
		sumMaxMem += info.maxmem
		sumRx += info.rx * 1024 / 8
		sumTx += info.tx * 1024 / 8

		if i >= min(this.rows, len(this.topInfos)) {
			continue
		}

		l := fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s|%6.1f|%s|%s",
			info.host, info.port,
			gofmt.Comma(info.dbsize), gofmt.Comma(info.conns), gofmt.Comma(info.ops),
			gofmt.ByteSize(info.mem), gofmt.ByteSize(info.maxmem),
			100.*info.memp,
			gofmt.ByteSize(info.rx*1024/8), gofmt.ByteSize(info.tx*1024/8))
		if this.beep > 0 {
			var val int64
			switch this.selectedCol() {
			case "conns":
				val = info.conns
			case "rx":
				val = info.rx
			case "tx":
				val = info.tx
			case "dbsize":
				val = info.dbsize
			case "ops":
				val = info.ops
			case "mem":
				val = info.mem
			case "maxm":
				val = info.maxmem

			}

			if val > this.beep {
				//l += "\a"
			}
		}
		lines = append(lines, l)

	}
	lines = append(lines, fmt.Sprintf("-TOTAL-|%d|%s|%s|%s|%s|%s|%6.1f|%s|%s",
		len(this.topInfos),
		gofmt.Comma(sumDbsize), gofmt.Comma(sumConns), gofmt.Comma(sumOps),
		gofmt.ByteSize(sumMem), gofmt.ByteSize(sumMaxMem),
		100.*float64(sumMem)/float64(sumMaxMem),
		gofmt.ByteSize(sumRx), gofmt.ByteSize(sumTx)))

	for row, line := range lines {
		if row == 0 {
			this.drawRow(line, row, termbox.ColorDefault, termbox.ColorBlue)
		} else if row == len(lines)-1 {
			this.drawRow(line, row, termbox.ColorYellow, termbox.ColorDefault)
		} else {
			this.drawRow(line, row, termbox.ColorDefault, termbox.ColorDefault)
		}
	}

	termbox.Flush()
}

func (this *Redis) updateRedisInfo(wg *sync.WaitGroup, host string, port int) {
	defer wg.Done()

	spec := redis.DefaultSpec().Host(host).Port(port)
	client, err := redis.NewSynchClientWithSpec(spec)
	if err != nil {
		return
	}
	defer client.Quit()

	infoMap, err := client.Info()
	if err != nil {
		return
	}

	maxMem, _ := client.MaxMemory()
	if maxMem == 0 {
		maxMem = 1 // FIXME
	}

	dbSize, _ := client.Dbsize()
	conns, _ := strconv.ParseInt(infoMap["connected_clients"], 10, 64)
	ops, _ := strconv.ParseInt(infoMap["instantaneous_ops_per_sec"], 10, 64)
	mem, _ := strconv.ParseInt(infoMap["used_memory"], 10, 64)
	rxKbps, _ := strconv.ParseFloat(infoMap["instantaneous_input_kbps"], 64)
	txKbps, _ := strconv.ParseFloat(infoMap["instantaneous_output_kbps"], 64)

	if this.ipInNum {
		host = ipaddr(host)
	}

	this.mu.Lock()
	this.topInfos = append(this.topInfos, redisTopInfo{
		host:   host,
		port:   port,
		dbsize: dbSize,
		ops:    ops,
		mem:    mem,
		maxmem: maxMem,
		memp:   float64(mem) / float64(maxMem),
		rx:     int64(rxKbps),
		tx:     int64(txKbps),
		conns:  conns,
	})
	this.mu.Unlock()
}

func (this *Redis) runPing(zkzone *zk.ZkZone) {
	var wg sync.WaitGroup
	allRedis := zkzone.AllRedis()
	this.topInfos = make([]redisTopInfo, 0, len(allRedis))

	for _, hostPort := range allRedis {
		host, port, err := net.SplitHostPort(hostPort)
		if err != nil {
			this.Ui.Error(hostPort)
			continue
		}

		nport, err := strconv.Atoi(port)
		if err != nil || nport < 0 {
			this.Ui.Error(hostPort)
			continue
		}

		wg.Add(1)
		go func(wg *sync.WaitGroup, host string, port int) {
			defer wg.Done()

			t0 := time.Now()

			spec := redis.DefaultSpec().Host(host).Port(port)
			client, err := redis.NewSynchClientWithSpec(spec)
			if err != nil {
				this.Ui.Error(fmt.Sprintf("[%s:%d] %v", host, port, err))
				return
			}
			defer client.Quit()

			if err := client.Ping(); err != nil {
				this.Ui.Error(fmt.Sprintf("[%s:%d] %v", host, port, err))
				return
			}

			latency := time.Since(t0)

			this.mu.Lock()
			this.topInfos = append(this.topInfos, redisTopInfo{
				host:    host,
				port:    port,
				t0:      t0,
				latency: latency,
			})
			this.mu.Unlock()
		}(&wg, host, nport)
	}
	wg.Wait()

	latency := metrics.NewRegisteredHistogram("redis.latency", metrics.DefaultRegistry, metrics.NewExpDecaySample(1028, 0.015))

	sortutil.AscByField(this.topInfos, "latency")
	lines := []string{"Host|Port|latency"}
	for _, info := range this.topInfos {
		latency.Update(info.latency.Nanoseconds() / 1e6)

		lines = append(lines, fmt.Sprintf("%s|%d|%s",
			info.host, info.port, info.latency))
	}
	this.Ui.Output(columnize.SimpleFormat(lines))

	// summary
	ps := latency.Percentiles([]float64{0.90, 0.95, 0.99, 0.999})
	this.Ui.Info(fmt.Sprintf("N:%d Min:%dms Max:%dms Mean:%.1fms 90%%:%.1fms 95%%:%.1fms 99%%:%.1fms",
		latency.Count(), latency.Min(), latency.Max(), latency.Mean(), ps[0], ps[1], ps[2]))
}

func (*Redis) Synopsis() string {
	return "Monitor redis instances"
}

func (this *Redis) Help() string {
	help := fmt.Sprintf(`
Usage: %s redis [options]

    %s

    -z zone

    -list

    -host 1|2
      Work with -list, print host instead of redis instance
      1: only display host
      2: 0 + port info

    -top
      Monitor all redis instances ops

    -freeze n
      TopN rows to freeze.
      Press 'f' to freeze, 'F' to unfreeze

    -port comma seperated port
      Work with -top to limit redis instances
      e,g. 10511,10522

    -n
      Show network addresses as numbers

    -sleep interval
      Sleep between -top refreshing screen. Defaults 7s
      e,g 10s

    -beep threshold

    -ping
      Ping all redis instances
    
    -add host:port

    -del host:port

`, this.Cmd, this.Synopsis())
	return strings.TrimSpace(help)
}