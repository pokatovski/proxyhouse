package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "expvar"

	"github.com/marpaia/graphite-golang"
	"github.com/recoilme/graceful"
	"github.com/recoilme/pudge"
	"github.com/tidwall/evio"
)

var (
	errClose       = errors.New("Error closed")
	version        = "0.1.8"
	port           = flag.Int("p", 8124, "TCP port number to listen on (default: 8124)")
	keepalive      = flag.Int("keepalive", 10, "keepalive connection, in seconds")
	readtimeout    = flag.Int("readtimeout", 5, "request header read timeout, in seconds")
	fwd            = flag.String("fwd", "http://localhost:8123", "forward to this server (clickhouse)")
	repl           = flag.String("repl", "", "replace this string on forward")
	delim          = flag.String("delim", ",", "body delimiter")
	syncsec        = flag.Int("syncsec", 2, "sync interval, in seconds")
	graphitehost   = flag.String("graphitehost", "", "graphite host")
	graphiteport   = flag.Int("graphiteport", 2023, "graphite port")
	graphiteprefix = flag.String("graphiteprefix", "relap.count.proxyhouse", "graphite prefix")
	grayloghost    = flag.String("grayloghost", "", "graylog host")
	graylogport    = flag.Int("graylogport", 12201, "graylog port")
	isdebug        = flag.Bool("isdebug", false, "debug requests")
	resendint      = flag.Int("resendint", 60, "resend error interval, in steps")
	warnlevel      = flag.Int("w", 400, "error counts for warning level")
	critlevel      = flag.Int("c", 500, "error counts for error level")

	status           = "OK\r\n"
	graylog *Graylog = nil
)

type conn struct {
	is   evio.InputStream
	addr string
}

type Store struct {
	sync.RWMutex
	Req          map[string][]byte
	cancelSyncer context.CancelFunc
}

var store = &Store{Req: make(map[string][]byte, 0)}
var totalConnections uint32 // Total number of connections opened since the server started running
var currConnections int32   // Number of open connections
var idleConnections int32   // Number of idle connections
var in uint32               //in requests
var out uint32              //out requests
var errorsCheck uint32      // Number of errors Check
var gr *graphite.Graphite
var buffersize = 1024 * 8
var hostname string

func main() {
	flag.Parse()
	//fix http client
	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = 1000

	store.backgroundManager(*syncsec)

	atomic.StoreUint32(&totalConnections, 0)
	atomic.StoreInt32(&currConnections, 0)
	atomic.StoreInt32(&idleConnections, 0)
	atomic.StoreUint32(&in, 0)
	atomic.StoreUint32(&out, 0)
	atomic.StoreUint32(&errorsCheck, 0)

	if *graphitehost != "" {
		g, err := graphite.NewGraphiteUDP(*graphitehost, *graphiteport)
		if err != nil {
			panic(err)
		}
		gr = g
	} else {
		gr = graphite.NewGraphiteNop(*graphitehost, *graphiteport)
	}
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	hostname = strings.ReplaceAll(host, ".", "_")

	if *grayloghost != "" {
		graylog = NewGraylog(Graylog{Host: *grayloghost, Port: *graylogport})
		graylog.Info("Start proxyhouse")
	}

	letspanic := checkErr()
	if letspanic != nil {
		panic(letspanic)
	}

	// Wait for interrupt signal to gracefully shutdown the server with
	// setup signal catching
	quit := make(chan os.Signal, 1)
	fallback := func() error {
		fmt.Println("Some signal - ignored")
		grlog(LEVEL_INFO, "Some signal - ignored")
		return nil
	}
	graceful.Unignore(quit, fallback, graceful.Terminate...)

	server := &http.Server{
		Addr:              ":" + fmt.Sprint(*port),
		ReadHeaderTimeout: time.Duration(*readtimeout) * time.Second,
		IdleTimeout:       time.Duration(*keepalive) * time.Second,
		ConnState:         statelistener,
	}
	http.HandleFunc("/", dorequest)
	http.HandleFunc("/status", showstatus)
	http.HandleFunc("/statistic", showstatistic)
	err = server.ListenAndServe()
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
		os.Exit(1)
	}
}

func grlog(level uint8, data ...interface{}) {
	if graylog != nil {
		graylog.Log(level, data...)
	} else {
		fmt.Println(data...)
	}
}

func dorequest(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Error(w, "404 not found.", http.StatusNotFound)
		return
	}

	switch r.Method {
	case "GET":
		date := time.Now().UTC().Format(http.TimeFormat)
		w.Header().Set("Date", date)
		w.Header().Set("Server", "proxyhouse "+version)
		w.Header().Set("Connection", "Closed")
		fmt.Fprint(w, "status = \"OK\"\r\n")
		return

	case "POST":
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer r.Body.Close()
		uri := r.URL.RawPath + "?" + r.URL.RawQuery
		if len(body) > 0 {
			delimiter := []byte(*delim)
			q := r.URL.Query().Get("query")
			if strings.HasSuffix(q, "FORMAT TSV") || strings.HasSuffix(q, "FORMAT CSV") {
				delimiter = []byte("")
			}
			store.Lock()
			_, ok := store.Req[uri]
			if !ok {
				store.Req[uri] = make([]byte, 0, buffersize)
			} else {
				store.Req[uri] = append(store.Req[uri], delimiter...)
			}
			store.Req[uri] = append(store.Req[uri], body...)

			store.Unlock()
			atomic.AddUint32(&in, 1)
			gr.SimpleSend(fmt.Sprintf("%s.requests_received", *graphiteprefix), "1")
			gr.SimpleSend(fmt.Sprintf("%s.byhost.%s.requests_received", *graphiteprefix, hostname), "1")
			table := extractTable(uri)
			gr.SimpleSend(fmt.Sprintf("%s.bytable.%s.requests_received", *graphiteprefix, table), "1")
			gr.SimpleSend(fmt.Sprintf("%s.bytes_received", *graphiteprefix), fmt.Sprintf("%d", len(body)))
			gr.SimpleSend(fmt.Sprintf("%s.byhost.%s.bytes_received", *graphiteprefix, hostname), fmt.Sprintf("%d", len(body)))
			gr.SimpleSend(fmt.Sprintf("%s.bytable.%s.bytes_received", *graphiteprefix, table), fmt.Sprintf("%d", len(body)))
			w.Header().Set("Server", "proxyhouse "+version)
			w.Header().Set("Content-type", "text/tab-separated-values; charset=UTF-8")
		} else {
			http.Error(w, "No data given.", http.StatusMethodNotAllowed)
		}

	default:
		http.Error(w, "Sorry, only GET and POST methods are supported.", http.StatusMethodNotAllowed)
	}
}

func showstatus(w http.ResponseWriter, r *http.Request) {
	errcount := 0
	list, err := filePathWalkDir("errors")
	if err == nil {
		errcount = len(list)
	}

	date := time.Now().UTC().Format(http.TimeFormat)
	w.Header().Set("Date", date)
	w.Header().Set("Server", "proxyhouse "+version)
	w.Header().Set("Connection", "Closed")
	if errcount >= *critlevel {
		w.WriteHeader(http.StatusInternalServerError)
	} else if errcount >= *warnlevel {
		w.WriteHeader(http.StatusBadRequest)
	}
	fmt.Fprintf(w, "status:%s", status)
}

func showstatistic(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "proxyhouse "+version)
	w.Header().Set("Connection", "Closed")
	fmt.Fprintf(w, "total connections:%d\r\n", atomic.LoadUint32(&totalConnections))
	fmt.Fprintf(w, "current connections:%d\r\n", atomic.LoadInt32(&currConnections))
	fmt.Fprintf(w, "idle connections:%d\r\n", atomic.LoadInt32(&idleConnections))
	fmt.Fprintf(w, "in requests:%d\r\n", atomic.LoadUint32(&in))
	fmt.Fprintf(w, "out requests:%d\r\n", atomic.LoadUint32(&out))
}

func statelistener(c net.Conn, cs http.ConnState) {
	switch cs {
	case http.StateNew:
		atomic.AddUint32(&totalConnections, 1)
		atomic.AddInt32(&currConnections, 1)
		atomic.AddInt32(&idleConnections, 1)
	case http.StateActive:
		atomic.AddInt32(&idleConnections, -1)
	case http.StateIdle:
		atomic.AddInt32(&idleConnections, 1)
	case http.StateClosed:
		atomic.AddInt32(&currConnections, -1)
		atomic.AddInt32(&idleConnections, -1)
	}
}

// backgroundManager runs continuously in the background and performs various
// operations such as forward requests.
func (store *Store) backgroundManager(interval int) {
	ctx, cancel := context.WithCancel(context.Background())
	store.cancelSyncer = cancel
	go func() {
		for {
			select {
			case <-ctx.Done():
				fmt.Println("backgroundManager - canceled")
				return
			default:
				atomic.AddUint32(&errorsCheck, 1)
				currCheck := atomic.LoadUint32(&errorsCheck)
				if currCheck%uint32(*resendint) == 0 {
					nopanic := checkErr()
					if nopanic != nil {
						fmt.Println("nopanic:", nopanic.Error())
					}
				}
				//keys reader
				store.RLock()
				keys := make([]string, 0)
				for key := range store.Req {
					keys = append(keys, key)
				}
				store.RUnlock()

				//keys itterator
				for _, key := range keys {
					//read as fast as possible and return mutex
					store.Lock()
					val := store.Req[key]
					//val := new(bytes.Buffer)
					//_, err := io.Copy(val, bytes.NewReader(store.Req[key]))
					send(key, val, true)
					delete(store.Req, key)
					store.Unlock()
					//send 2 ch
					atomic.AddUint32(&out, 1)

				}
				time.Sleep(time.Duration(interval) * time.Second)
			}
		}
	}()
}

func extractTable(key string) string {
	table := "unknown"
	lowkey := strings.ToLower(key)
	if strings.Contains(lowkey, "insert%20into%20") {
		from := strings.Index(lowkey, "insert%20into%20")
		if from >= 0 {
			from += len("insert%20into%20")
			to := strings.Index(lowkey[from:], "%20")
			if to > 0 {
				table = lowkey[from:(to + from)]
			}
		}
	}
	if table == "unknown" {
		if strings.Contains(lowkey, "insert+into+") {
			from := strings.Index(lowkey, "insert+into+")
			if from >= 0 {
				from += len("insert+into+")
				to := strings.Index(lowkey[from:], "+")
				if to > 0 {
					table = lowkey[from:(to + from)]
				}
			}
		}
	}
	return table
}

// вырезаем из строки password=xxxxx для логов
func hidePassword(str string) string {
	replace := "password="
	pos := strings.Index(str, replace)
	if pos < 0 {
		return str
	}
	pos2 := strings.Index(str[pos:], "&")
	if pos2 < 0 {
		return str[0:pos+len(replace)] + "*"
	}
	return str[0:pos+len(replace)] + "*" + str[pos+pos2:]
}

//sender
func send(key string, val []byte, silent bool) (err error) {
	if *isdebug {
		fmt.Printf("time:%s\tkey:%s\tval:%s\n", time.Now(), key, val)
	}
	//send
	table := extractTable(key)
	uri := key
	if strings.HasPrefix(uri, "/") {
		uri = *fwd + uri
	} else {
		uri = strings.Replace(uri, *repl, *fwd, 1)
	}
	req, err := http.NewRequest("POST", uri /*fmt.Sprintf("%s%s", *fwd, key)*/, bytes.NewBuffer(val))

	slices := bytes.Split(val, []byte(*delim))
	gr.SimpleSend(fmt.Sprintf("%s.rows_sent", *graphiteprefix), fmt.Sprintf("%d", len(slices)))
	gr.SimpleSend(fmt.Sprintf("%s.requests_sent", *graphiteprefix), "1")
	gr.SimpleSend(fmt.Sprintf("%s.byhost.%s.rows_sent", *graphiteprefix, hostname), fmt.Sprintf("%d", len(slices)))
	gr.SimpleSend(fmt.Sprintf("%s.byhost.%s.requests_sent", *graphiteprefix, hostname), "1")
	gr.SimpleSend(fmt.Sprintf("%s.bytable.%s.rows_sent", *graphiteprefix, table), fmt.Sprintf("%d", len(slices)))
	gr.SimpleSend(fmt.Sprintf("%s.bytable.%s.requests_sent", *graphiteprefix, table), "1")
	gr.SimpleSend(fmt.Sprintf("%s.bytes_sent", *graphiteprefix), fmt.Sprintf("%d", len(val)))
	gr.SimpleSend(fmt.Sprintf("%s.byhost.%s.bytes_sent", *graphiteprefix, hostname), fmt.Sprintf("%d", len(val)))
	gr.SimpleSend(fmt.Sprintf("%s.bytable.%s.bytes_sent", *graphiteprefix, table), fmt.Sprintf("%d", len(val)))

	if err != nil {
		gr.SimpleSend(fmt.Sprintf("%s.ch_errors", *graphiteprefix), "1")
		gr.SimpleSend(fmt.Sprintf("%s.byhost.%s.ch_errors", *graphiteprefix, hostname), "1")
		gr.SimpleSend(fmt.Sprintf("%s.bytable.%s.ch_errors", *graphiteprefix, table), "1")
		grlog(LEVEL_ERR, "Create request error: ", hidePassword(uri), " error: ", err)
		if silent && len(val) > 0 {
			db := fmt.Sprintf("errors/%d", time.Now().UnixNano())
			pudge.Set(db, key, val)
			pudge.Close(db)
		}
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil && resp.StatusCode != 200 {
		err = errors.New("Error: response code not 200")
	}
	if err != nil {
		grlog(LEVEL_ERR, "Request error: ", hidePassword(uri), " error: ", err)
		status = err.Error() + "\r\n"
		gr.SimpleSend(fmt.Sprintf("%s.ch_errors", *graphiteprefix), "1")
		gr.SimpleSend(fmt.Sprintf("%s.byhost.%s.ch_errors", *graphiteprefix, hostname), "1")
		gr.SimpleSend(fmt.Sprintf("%s.bytable.%s.ch_errors", *graphiteprefix, table), "1")
		if resp != nil {
			bodyResp, _ := ioutil.ReadAll(resp.Body)
			grlog(LEVEL_ERR, "Response: status: ", resp.StatusCode, " body: ", string(bodyResp))
		}
		if silent && len(val) > 0 {

			db := fmt.Sprintf("errors/%d", time.Now().UnixNano())
			pudge.Set(db, key, val)
			pudge.Close(db)
		}
		return
	} else {
		status = "OK\r\n"
	}
	defer resp.Body.Close()
	return
}

func checkErr() (err error) {
	list, err := filePathWalkDir("errors")
	if err != nil {
		if err.Error() != "lstat errors: no such file or directory" {
			return err
		}
		//send empty err if no errors
		return nil
	}
	sort.Sort(sort.StringSlice(list))
	for _, file := range list {
		db, err := pudge.Open("errors/"+file, nil)
		grlog(LEVEL_ERR, "Proccessing error:", file)
		if err != nil {
			return err
		}
		keys, err := db.Keys(nil, 0, 0, true)
		if err != nil {
			return err
		}
		for _, key := range keys {
			//println(key)
			var val []byte
			err := db.Get(key, &val)
			if err != nil {
				return err
			}
			//buf := new(bytes.Buffer)
			//io.Copy(buf, bytes.NewReader(val))
			err = send(string(key), val, false)

			if err != nil {
				return err
			}
		}
		db.DeleteFile()
		// sleep 3 seconds to prevent throttling CH
		time.Sleep(3 * time.Second)
	}
	return
}

func filePathWalkDir(root string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			grlog(LEVEL_ERR, "dirwalk error ", err)
			return err
		}
		if !info.IsDir() {
			if !strings.HasSuffix(path, ".idx") {
				files = append(files, filepath.Base(path))
			}

		}
		return nil
	})
	return files, err
}
