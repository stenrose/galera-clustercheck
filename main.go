package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/coreos/go-systemd/daemon"
	_ "github.com/go-sql-driver/mysql"
)

const (
	STATE_JOINING = 1
	STATE_DONOR   = 2
	STATE_JOINED  = 3
	STATE_SYNCED  = 4
)

var (
	username              = flag.String("username", "", "MySQL username")
	password              = flag.String("password", "", "MySQL password")
	iniFile               = flag.String("inifile", "/etc/galera-clustercheck/my.cnf", "MySQL Option file")
	socket                = flag.String("socket", "/run/mysqld/mysqld.sock", "MySQL Unix socket")
	host                  = flag.String("host", "", "MySQL server")
	port                  = flag.Int("port", 3306, "MySQL port")
	timeout               = flag.String("timeout", "10s", "MySQL connection timeout")
	availableWhenDonor    = flag.Bool("donor", false, "Cluster available while node is a donor")
	availableWhenReadonly = flag.Bool("readonly", false, "Cluster available while node is read only")
	requireMaster         = flag.Bool("requiremaster", false, "Cluster available only while node is master")
	bindAddr              = flag.String("bindaddr", "", "Clustercheck bind address")
	bindPort              = flag.Int("bindport", 8000, "Clustercheck bind port")
	debug                 = flag.Bool("debug", false, "Debug mode. Will also print successfull 200 HTTP responses to stdout")
	forceUp               = false
	forceDown             = false
	dataSourceName        = ""
)

func main() {
	flag.Parse()

	if *username == "" && *password == "" {
		parseConfigFile()
	}

	if *host != "" {
		dataSourceName = fmt.Sprintf("%s:%s@tcp(%s:%d)/?timeout=%s", *username, *password, *host, *port, *timeout)
	} else {
		dataSourceName = fmt.Sprintf("%s:%s@unix(%s)/?timeout=%s", *username, *password, *socket, *timeout)
	}

	db, err := sql.Open("mysql", dataSourceName)
	if err != nil {
		panic(err.Error())
	}

	db.SetMaxIdleConns(10)
	db.SetMaxOpenConns(10)

	readOnlyStmt, err := db.Prepare("SHOW GLOBAL VARIABLES LIKE 'read_only'")
	if err != nil {
		log.Fatal(err)
	}

	wsrepLocalStateStmt, err := db.Prepare("SHOW GLOBAL STATUS LIKE 'wsrep_local_state'")
	if err != nil {
		log.Fatal(err)
	}

	wsrepLocalIndexStmt, err := db.Prepare("SHOW GLOBAL STATUS LIKE 'wsrep_local_index'")
	if err != nil {
		log.Fatal(err)
	}

	checker := &Checker{wsrepLocalIndexStmt, wsrepLocalStateStmt, readOnlyStmt}

	log.Println("Listening...")
	http.HandleFunc("/", checker.Root)
	http.HandleFunc("/master", checker.Master)
	http.HandleFunc("/up", checker.Up)
	http.HandleFunc("/down", checker.Down)
	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)
	log.Fatal(http.ListenAndServe(fmt.Sprintf("%s:%d", *bindAddr, *bindPort), nil))
}

func parseConfigFile() {
	content, err := ioutil.ReadFile(*iniFile)
	if err != nil {
		log.Fatalf("error reading config: %v", err)
	}
	lines := strings.Split(string(content), "\n")

	for _, line := range lines {
		if len(line) > 3 && line[0:4] == "user" {
			tmp := strings.Split(line, "=")
			*username = strings.Trim(tmp[1], " ")
		}
		if len(line) > 7 && line[0:8] == "password" {
			tmp := strings.Split(line, "=")
			*password = strings.Trim(tmp[1], " ")
		}
	}
}

type Checker struct {
	wsrepLocalIndexStmt *sql.Stmt
	wsrepLocalStateStmt *sql.Stmt
	readOnlyStmt        *sql.Stmt
}

func (c *Checker) Root(w http.ResponseWriter, r *http.Request) {
	c.Clustercheck(w, r, *requireMaster, forceUp, forceDown)
}

func (c *Checker) Master(w http.ResponseWriter, r *http.Request) {
	c.Clustercheck(w, r, true, forceUp, forceDown)
}

func (c *Checker) Up(w http.ResponseWriter, r *http.Request) {
	c.Clustercheck(w, r, *requireMaster, true, forceDown)
}

func (c *Checker) Down(w http.ResponseWriter, r *http.Request) {
	c.Clustercheck(w, r, *requireMaster, forceUp, true)
}

func (c *Checker) Clustercheck(w http.ResponseWriter, r *http.Request, requireMaster, forceUp, forceDown bool) {
	var fieldName, readOnly string
	var wsrepLocalState int
	var wsrepLocalIndex int

	remoteIp, _, _ := net.SplitHostPort(r.RemoteAddr)

	if forceUp {
		if *debug {
			log.Println(remoteIp, "Node available by forceUp")
		}
		fmt.Fprint(w, "Node available by forceUp\n")
		return
	}

	if forceDown {
		if *debug {
			log.Println(remoteIp, "Node unavailable by forceDown")
		}
		http.Error(w, "Node unavailable by forceDown", http.StatusServiceUnavailable)
		return
	}

	readOnlyErr := c.readOnlyStmt.QueryRow().Scan(&fieldName, &readOnly)
	if readOnlyErr != nil {
		log.Println(remoteIp, readOnlyErr.Error())
		http.Error(w, "Error while running readOnlyStmt", http.StatusInternalServerError)
		return
	}

	if readOnly == "ON" && !*availableWhenReadonly {
		log.Println(remoteIp, "Node is read_only")
		http.Error(w, "Node is read_only", http.StatusServiceUnavailable)
		return
	}

	wsrepLocalStateErr := c.wsrepLocalStateStmt.QueryRow().Scan(&fieldName, &wsrepLocalState)
	if wsrepLocalStateErr != nil {
		log.Println(remoteIp, wsrepLocalStateErr.Error())
		http.Error(w, "Error while running wsrepLocalStateStmt", http.StatusInternalServerError)
		return
	}

	switch wsrepLocalState {
	case STATE_JOINING:
		if *debug {
			log.Println(remoteIp, "Node in Joining state")
		}
		http.Error(w, "Node in Joining state", http.StatusServiceUnavailable)
		return
	case STATE_DONOR:
		if *availableWhenDonor {
			if *debug {
				log.Println(remoteIp, "Node in Donor state")
			}
			fmt.Fprint(w, "Node in Donor state\n")
			return
		} else {
			if *debug {
				log.Println(remoteIp, "Node in Donor state")
			}
			http.Error(w, "Node in Donor state", http.StatusServiceUnavailable)
			return
		}
	case STATE_JOINED:
		if *debug {
			log.Println(remoteIp, "Node in Joined state")
		}
		http.Error(w, "Node in Joined state", http.StatusServiceUnavailable)
		return
	case STATE_SYNCED:
		if requireMaster {
			wsrepLocalIndexErr := c.wsrepLocalIndexStmt.QueryRow().Scan(&fieldName, &wsrepLocalIndex)
			if wsrepLocalIndexErr != nil {
				log.Println(remoteIp, wsrepLocalIndexErr.Error())
				http.Error(w, "Error while running wsrepLocalIndexStmt", http.StatusInternalServerError)
				return
			}
			if wsrepLocalIndex == 0 {
				if *debug {
					log.Println(remoteIp, "Node in Synced state and 'wsrep_local_index==0'")
				}
				fmt.Fprintf(w, "Node in Synced state and 'wsrep_local_index==0'\n")
				return
			} else if wsrepLocalIndex != 0 {
				if *debug {
					log.Println(remoteIp, "Node in Synced state but not 'wsrep_local_index==0'")
				}
				http.Error(w, "Node in Synced state but not 'wsrep_local_index==0'", http.StatusServiceUnavailable)
				return
			}
		}
		if *debug {
			log.Println(remoteIp, "Node in Synced state")
		}
		fmt.Fprint(w, "Node in Synced state\n")
		return
	default:
		if *debug {
			log.Println(remoteIp, fmt.Sprintf("Node in an unknown state (%d)", wsrepLocalState))
		}
		http.Error(w, fmt.Sprintf("Node in an unknown state (%d)", wsrepLocalState), http.StatusServiceUnavailable)
		return
	}
}
