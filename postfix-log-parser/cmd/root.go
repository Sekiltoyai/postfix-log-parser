package cmd

import (
	"os"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
	"bufio"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"syscall"
	"os/signal"
	"encoding/json"

	"github.com/spf13/cobra"
	"github.com/tabalt/pidfile"
	postfixlog "github.com/yo000/postfix-log-parser"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func init() {}

var (
	File   os.File
	Writer *bufio.Writer
	outfMtx sync.Mutex

	Version = "1.4.5"

	BuildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postfixlogparser_build_info",
		Help: "Constant 1 value labeled by version and goversion from which postfix-log-parser was built",
	}, []string{"version", "goversion"})
	StartTime = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "postfixlogparser_time_start_seconds",
		Help: "Process start time in UNIX timestamp (seconds)",
	})
	LineReadCnt = promauto.NewCounter(prometheus.CounterOpts{
		Name: "postfixlogparser_line_read_count",
		Help: "Number of lines read",
	})
	LineIncorrectCnt = promauto.NewCounter(prometheus.CounterOpts{
		Name: "postfixlogparser_line_incorrect_count",
		Help: "Number of lines with incorrect format",
	})
	LineOutCnt = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "postfixlogparser_line_out_count",
		Help: "Number of lines written to ouput",
	}, []string{"host"})
	MsgInCnt = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "postfixlogparser_msg_in_count",
		Help: "Number of mails accepted by smtpd",
	}, []string{"host"})
	MsgSentCnt = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "postfixlogparser_msg_sent_count",
		Help: "Number of mails sent",
	}, []string{"host"})
	MsgDeferredCnt = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "postfixlogparser_msg_deferred_count",
		Help: "Number of mails deferred",
	}, []string{"host"})
	MsgBouncedCnt = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "postfixlogparser_msg_bounced_count",
		Help: "Number of mails bounced",
	}, []string{"host"})
	MsgRejectedCnt = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "postfixlogparser_msg_rejected_count",
		Help: "Number of mails rejected",
	}, []string{"host"})
	MsgHoldCnt = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "postfixlogparser_msg_hold_count",
		Help: "Number of mails hold",
	}, []string{"host"})
	MsgAuthFailed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "postfixlogparser_auth_failed_count",
		Help: "Number of failed authentications",
	}, []string{"host"})
	ConnectedClientCnt = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "postfixlogparser_client_count",
		Help: "Number of connected clients",
	})

	rootCmd = &cobra.Command{
		Use:   "postfix-log-parser",
		Short: "Postfix Log Parser v" + Version + ". Parse postfix log, and output json format",
		//Long: ``,
		Run: func(cmd *cobra.Command, args []string) {
			processLogs(cmd, args)
		},
	}

	gFlatten             bool
	gOutputFile          string
	gPidFilePath         string
	gSyslogListenAddress string
	gPromListenAddress   string
	gPromMetricPath      string
)

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		rootCmd.SetOutput(os.Stderr)
		rootCmd.Println(err)
		os.Exit(1)
	}
}


func NewWriter(file string) (*bufio.Writer, *os.File, error) {
	if len(file) > 0 {
		var f *os.File
		var err error
		if _, err = os.Stat(file); err == nil {
			f, err = os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0640)
		} else if os.IsNotExist(err) {
			f, err = os.OpenFile(file, os.O_CREATE|os.O_WRONLY, 0640)
		}
		if err != nil {
			return nil, nil, err
		}
		Writer = bufio.NewWriter(f)
		return Writer, f, nil
	} else {
		Writer = bufio.NewWriter(os.Stdout)
		return Writer, nil, nil
	}
}

func writeOut(msg string, filename string) error {
	_, err := fmt.Fprintln(Writer, msg)
	Writer.Flush()
	if err != nil {
		return err
	}

	var tmpPlp postfixlog.PostfixLogParser
	json.Unmarshal([]byte(msg), &tmpPlp)
	LineOutCnt.WithLabelValues(tmpPlp.Hostname).Inc()

	return nil
}

func outputCb(value interface{}) error {
	jsonBytes, err := json.Marshal(value)
	if err != nil {
		return err
	}

	outfMtx.Lock()
	err = writeOut(string(jsonBytes), gOutputFile)
	outfMtx.Unlock()
	if err != nil {
		return err
	}

	return nil
}

func statsCb(statname string, hostname string, inc bool, value float64) {
	var c prometheus.Counter
	switch statname {
	case "lineincorrectcnt":
		c = LineIncorrectCnt
	case "msgincnt":
		c = MsgInCnt.WithLabelValues(hostname)
	case "msgdeferredcnt":
		c = MsgDeferredCnt.WithLabelValues(hostname)
	case "msgsentcnt":
		c = MsgSentCnt.WithLabelValues(hostname)
	case "msgrejectedcnt":
		c = MsgRejectedCnt.WithLabelValues(hostname)
	case "msgholdcnt":
		c = MsgHoldCnt.WithLabelValues(hostname)
	case "msgbouncedcnt":
		c = MsgBouncedCnt.WithLabelValues(hostname)
	case "msgauthfailed":
		c = MsgAuthFailed.WithLabelValues(hostname)
	default:
		return
	}

	if c != nil {
		if inc {
			c.Inc()
		} else {
			c.Add(value)
		}
	}
}

// Every 24H, remove sent, milter-rejected and deferred that entered queue more than 5 days ago
func periodicallyCleanMQueue(pp *postfixlog.PostfixLogProcessor) {
	for range time.Tick(time.Hour * 24) {
		pp.CleanMQueue(5 * 24 * time.Hour)
	}
}

func initConfig() {}

func init() {

	rootCmd.Version = Version

	rootCmd.Flags().BoolVarP(&gFlatten, "gFlatten", "f", false, "Flatten output for using with syslog")
	rootCmd.Flags().StringVarP(&gOutputFile, "out", "o", "", "Output to file, append if exists")
	rootCmd.Flags().StringVarP(&gPidFilePath, "pidfile", "p", "", "pid file path")
	rootCmd.Flags().StringVarP(&gSyslogListenAddress, "syslog.listen-address", "s", "do-not-listen", "Address to listen on for syslog incoming messages. Default is to parse stdin")
	rootCmd.Flags().StringVarP(&gPromListenAddress, "prom.listen-address", "l", "do-not-listen", "Address to listen on for prometheus metrics")
	rootCmd.Flags().StringVarP(&gPromMetricPath, "prom.telemetry-path", "m", "/metrics", "Path under which to expose metrics.")

	cobra.OnInitialize(initConfig)
}

func scanAndProcess(scanner *bufio.Scanner, isStdin bool, conn net.Conn,
	pp *postfixlog.PostfixLogProcessor) error {
	for {
		// If input is made via TCP Conn, we need to read from a connected net.Conn
		if scanner == nil || (isStdin == false && conn == nil) {
			return errors.New("Invalid input")
		}

		if false == scanner.Scan() {
			// After Scan returns false, the Err method will return any error that occurred during scanning, except that if it was io.EOF, Err will return nil
			if err := scanner.Err(); err != nil {
				log.Printf("Error reading data: %v\n", err.Error())
			}
			if isStdin == false {
				log.Printf("No more data, closing connection.\n")
				// Should we?
				conn.Close()
			}
			// input is dead, abort mission!
			return errors.New("Read error")
		}
		// Extend timeout after successful read (so we got an idle timeout)
		if isStdin == false && conn != nil {
			conn.SetReadDeadline(time.Now().Add(time.Duration(600) * time.Second))
		}

		LineReadCnt.Inc()

		read := scanner.Bytes()
		err := pp.ParseStoreAndWrite(read)
		if err != nil {
			if err.Error() != "Error: Line do not match regex" {
				return err
			} else {
				log.Printf("input do not match regex: %s\n", string(read))
			}
		}
	}
	return nil
}

func processLogs(cmd *cobra.Command, args []string) {
	//var scanner *bufio.Scanner
	var listener net.Listener
	var useStdin bool

	BuildInfo.WithLabelValues(Version, runtime.Version()).Set(1)
	StartTime.Set(float64(time.Now().Unix()))

	// Prometheus exporter
	if gPromListenAddress != "do-not-listen" {
		go func() {
			http.Handle(gPromMetricPath, promhttp.Handler())
			http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`
				<html>
				<head><title>Postfix-log-parser Exporter</title></head>
				<body>
				<h1>Postfix-log-parser Exporter</h1>
				<p><a href='` + gPromMetricPath + `'>Metrics</a></p>
				</body>
				</html>`))
			})
			log.Fatal(http.ListenAndServe(gPromListenAddress, nil))
		}()
	}

	// Create PID file
	if len(gPidFilePath) > 0 {
		if pid, err := pidfile.Create(gPidFilePath); err != nil {
			log.Fatal(err)
		} else {
			defer pid.Clear()
		}
	}

	// initialize
	pp := postfixlog.NewPostfixLogProcessor(outputCb, statsCb, gFlatten)

	// Get a writer, file or stdout
	_, File, err := NewWriter(gOutputFile)
	if err != nil {
		cmd.SetOutput(os.Stderr)
		cmd.Println(err)
		os.Exit(1)
	}

	// Manage output file rotation when receiving SIGUSR1
	if len(gOutputFile) > 0 {
		sig := make(chan os.Signal)
		signal.Notify(sig, syscall.SIGUSR1)
		go func() {
			for {
				<-sig
				outfMtx.Lock()
				fmt.Println("SIGUSR1 received, recreating output file")
				File.Close()
				_, File, err = NewWriter(gOutputFile)
				if err != nil {
					outfMtx.Unlock()
					cmd.SetOutput(os.Stderr)
					cmd.Println(err)
					os.Exit(1)
				}
				outfMtx.Unlock()
			}
		}()
	}

	// Cleaner thread
	go periodicallyCleanMQueue(pp)


	// On demand Mqueue cleaning... For debug, dont try this at home, kids!
/*	sig2 := make(chan os.Signal)
	signal.Notify(sig2, syscall.SIGUSR2)
	go func() {
		for {
			<-sig2
			cleanMQueue(mQueue, &mqMtx, 1 * time.Hour)
		}
	}()
*/	

	// Initialize Stdin input...
	if true == strings.EqualFold(gSyslogListenAddress, "do-not-listen") {
		useStdin = true
		scanner := bufio.NewScanner(os.Stdin)
		scanAndProcess(scanner, useStdin, nil, pp)
		// ...or manages incoming connections
	} else {
		listener, err = net.Listen("tcp", gSyslogListenAddress)
		if err != nil {
			log.Fatal(fmt.Sprintf("Error listening on %s: %v\n", gSyslogListenAddress, err))
		}
		for {
			connClt, err := listener.Accept()
			if err != nil {
				log.Printf("Error accepting: %v", err)
				// Loop
				continue
			}
			scanner := bufio.NewScanner(connClt)
			ConnectedClientCnt.Inc()
			go scanAndProcess(scanner, useStdin, connClt, pp)
			ConnectedClientCnt.Dec()
		}
	}

	if File != nil {
		outfMtx.Lock()
		File.Close()
		outfMtx.Unlock()
	}
}
