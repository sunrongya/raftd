package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/goraft/raft"
	"github.com/goraft/raftd/command"
	"github.com/goraft/raftd/db"
	"github.com/gorilla/mux"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"path/filepath"
	"sync"
	"time"
	"net/url"
	"net/http/httputil"
	"errors"
)

// The raftd server is a combination of the Raft server and an HTTP
// server which acts as the transport.
type Server struct {
	name       string
	host       string
	port       int
	path       string
	router     *mux.Router
	raftServer raft.Server
	httpServer *http.Server
	db         *db.DB
	mutex      sync.RWMutex
}

// Creates a new server.
func New(path string, host string, port int) *Server {
	s := &Server{
		host:   host,
		port:   port,
		path:   path,
		db:     db.New(),
		router: mux.NewRouter(),
	}

	// Read existing name or generate a new one.
	if b, err := ioutil.ReadFile(filepath.Join(path, "name")); err == nil {
		s.name = string(b)
	} else {
		s.name = fmt.Sprintf("%07x", rand.Int())[0:7]
		if err = ioutil.WriteFile(filepath.Join(path, "name"), []byte(s.name), 0644); err != nil {
			panic(err)
		}
	}

	return s
}

// Returns the connection string.
func (s *Server) connectionString() string {
	return fmt.Sprintf("http://%s:%d", s.host, s.port)
}

// Starts the server.
func (s *Server) ListenAndServe(leader string) error {
	var err error

	log.Printf("Initializing Raft Server: %s", s.path)

	// Initialize and start Raft server.
	transporter := raft.NewHTTPTransporter("/raft", 200*time.Millisecond)
	s.raftServer, err = raft.NewServer(s.name, s.path, transporter, nil, s.db, "")
	if err != nil {
		log.Fatal(err)
	}
	transporter.Install(s.raftServer, s)
	s.raftServer.Start()

	if leader != "" {
		// Join to leader if specified.

		log.Println("Attempting to join leader:", leader)

		if !s.raftServer.IsLogEmpty() {
			log.Fatal("Cannot join with an existing log")
		}
		if err := s.Join(leader); err != nil {
			log.Fatal(err)
		}

	} else if s.raftServer.IsLogEmpty() {
		// Initialize the server by joining itself.

		log.Println("Initializing new cluster")

		_, err := s.raftServer.Do(&raft.DefaultJoinCommand{
			Name:             s.raftServer.Name(),
			ConnectionString: s.connectionString(),
		})
		if err != nil {
			log.Fatal(err)
		}

	} else {
		log.Println("Recovered from log")
	}

	log.Println("Initializing HTTP server")

	// Initialize and start HTTP server.
	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: s.router,
	}

	s.router.HandleFunc("/db/{key}", s.readHandler).Methods("GET")
	s.router.HandleFunc("/db/{key}", s.writeHandler).Methods("POST")
	s.router.HandleFunc("/join", s.joinHandler).Methods("POST")

	log.Println("Listening at:", s.connectionString())

	return s.httpServer.ListenAndServe()
}

// This is a hack around Gorilla mux not providing the correct net/http
// HandleFunc() interface.
func (s *Server) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	s.router.HandleFunc(pattern, handler)
}

// Joins to the leader of an existing cluster.
func (s *Server) Join(leader string) error {
	command := &raft.DefaultJoinCommand{
		Name:             s.raftServer.Name(),
		ConnectionString: s.connectionString(),
	}

	var b bytes.Buffer
	if err := json.NewEncoder(&b).Encode(command); err != nil {
		return nil
	}

	resp, err := http.Post(fmt.Sprintf("http://%s/join", leader), "application/json", &b)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// Look for redirect.
	if resp.StatusCode == http.StatusTemporaryRedirect {
		leader := resp.Header.Get("Location")
		if leader == "" {
			return errors.New("Redirect requested, but no location header supplied")
		}
		u, err := url.Parse(leader)
		if err != nil {
			return errors.New("Failed to parse redirect location")
		}
		log.Printf("Redirecting to leader at %s", u.Host)
		return s.Join(u.Host)
	}

	return nil
}

func (s *Server) joinHandler(w http.ResponseWriter, req *http.Request) {
	if s.raftServer.State() != "leader" {
		s.leaderRedirect(w, req)
		return
	}
	
	command := &raft.DefaultJoinCommand{}
	if err := json.NewDecoder(req.Body).Decode(&command); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := s.raftServer.Do(command); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (s *Server) readHandler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	value := s.db.Get(vars["key"])
	w.Write([]byte(value))
}

func (s *Server) writeHandler(w http.ResponseWriter, req *http.Request) {
	if s.raftServer.State() != "leader" {
		s.leaderProxy(w, req)
		//s.leaderRedirect(w, req)
		return
	}
	
	vars := mux.Vars(req)

	// Read the value from the POST body.
	b, err := ioutil.ReadAll(req.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	value := string(b)

	// Execute the command against the Raft server.
	_, err = s.raftServer.Do(command.NewWriteCommand(vars["key"], value))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

// leaderRedirect returns a 307 Temporary Redirect, with the full path
// to the leader.
func (s *Server) leaderRedirect(w http.ResponseWriter, r *http.Request) {
	peers := s.raftServer.Peers()
	leader := peers[s.raftServer.Leader()]

	if leader == nil {
		// No leader available, give up.
		// log.Error("attempted leader redirection, but no leader available")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("no leader available"))
		return
	}

	var u string
	for _, p := range peers {
		if p.Name == leader.Name {
			u = p.ConnectionString
			break
		}
	}
	http.Redirect(w, r, u+r.URL.Path, http.StatusTemporaryRedirect)
}

func (s *Server) leaderProxy(w http.ResponseWriter, r *http.Request) {
	peers := s.raftServer.Peers()
	leader := peers[s.raftServer.Leader()]

	if leader == nil {
		// No leader available, give up.
		// log.Error("attempted leader redirection, but no leader available")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("no leader available"))
		return
	}

	var u string
	for _, p := range peers {
		if p.Name == leader.Name {
			u = p.ConnectionString
			break
		}
	}
    
	targetUrl, err := url.Parse(u)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("no leader available Parse"))
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(targetUrl)
	proxy.ServeHTTP(w, r)
}
