package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	Attempts int = iota // Unique keys
	Retry
)

// Node holds the data about a backend server
type Node struct {
	URL          *url.URL
	Active       bool
	weight	     float64
	mutex        sync.RWMutex
	ReverseProxy *httputil.ReverseProxy
}

// NodePool holds slice of nodes and most recently used node index
type NodePool struct {
	nodes   []*Node
	current uint64
}

// AddNode new node to NodePool
func (np *NodePool) AddNode(n *Node) {
	np.nodes = append(np.nodes, n)
}

// NextIdx atomically increase the counter and return an index
func (np *NodePool) NextIdx() int {
	return int(atomic.AddUint64(&np.current, uint64(1)) % uint64(len(np.nodes)))
}

// isActive returns whether node is active or dead
func (n *Node) isActive() bool {
	var active bool
	n.mutex.RLock()
	active = n.Active
	n.mutex.RUnlock()
	return active
}

// getWeight returns the weight of the node
func (n *Node) getWeight() float64 {
	n.mutex.RLock()
	weight := n.weight
	n.mutex.RUnlock()
	return weight
}

//Swap two elements in nodePool
func (np *NodePool) Swap(i uint64, j uint64) {
	temp := np.nodes[i]
	np.nodes[i] = np.nodes[j]
	np.nodes[j] = temp
}

// Heapify will rearrange the max heap based on weights, takes index and if the node is root
func (np *NodePool) Heapify(idx uint64, root bool) {
	largest := idx
	left := 2*idx + 1
	right := 2*idx + 2

	if root {
		np.nodes[idx].weight /= 2
	}

	if left < uint64(len(np.nodes)) && np.nodes[left].isActive() && np.nodes[left].getWeight() > np.nodes[largest].getWeight() {
		largest = left
	}
	
	if right < uint64(len(np.nodes)) && np.nodes[right].isActive() && np.nodes[right].getWeight() > np.nodes[largest].getWeight() {
		largest = right
	}

	if largest != idx {
		if root {
			np.nodes[idx].weight *= 2
		}

		np.Swap(largest, idx)
		np.Heapify(largest, false)
	}

	if left < uint64(len(np.nodes)) && np.nodes[left].getWeight() < 1 {
		np.Heapify(left, false)
	}

	if right < uint64(len(np.nodes)) && np.nodes[right].getWeight() < 1 {
		np.Heapify(right, false)
	}

}

// NextNode find next active node
func (np *NodePool) NextNode() *Node {
	//// Round Robin algorithm
	//next := np.NextIdx()
	//for i := next; i < len(np.nodes)+next; i++ {
		//idx := i % len(np.nodes)
		//if np.nodes[idx].isActive() {
			//atomic.StoreUint64(&np.current, uint64(idx))
			//return np.nodes[idx]
		//}
	//}
	//return nil

	//Using heapify to select node
	return np.nodes[0]

}

// Balance incoming requests
func loadBalancer(w http.ResponseWriter, r *http.Request) {
	attempts := GetAttemptsFromContext(r)
	if attempts > 3 {
		log.Printf("%s(%s) Max attempts reached, terminating\n", r.RemoteAddr, r.URL.Path)
		http.Error(w, "Service not available", http.StatusServiceUnavailable)
		return
	}
	node := nodePool.NextNode()
	if node != nil {
		node.ReverseProxy.ServeHTTP(w, r)
		nodePool.Heapify(0, true)
		return
	}
	// 0 active nodes available
	http.Error(w, "Downtime: No nodes available", http.StatusServiceUnavailable)
}

// Status check node status by establishing TCP connection
func (n *Node) Status() bool {
	conn, err := net.DialTimeout("tcp", n.URL.Host, 2*time.Second)
	if err != nil {
		log.Println("Node unreachable: ", err)
		return false
	}
	_ = conn.Close()
	return true
}

// SetProps sets node's status and changes node's weight
func (n *Node) SetProps(status bool) {
	n.mutex.Lock()
	n.Active = status
	if !status {
		n.weight /= 3.0;
	} else if n.weight < 1 {
		n.weight *= 2.0; 
	} 
	n.mutex.Unlock()
}


// HealthCheck pings the node and update status
func (np *NodePool) HealthCheck() {
	for i, n := range np.nodes {
		status := n.Status()
		n.SetProps(status)
		msg := "active"
		if !status {
			msg = "dead"
			np.Heapify(uint64(i), false)
		}
		log.Printf("%s [%s] [%0.2g]\n", n.URL, msg, n.weight)
	}
}

// SetNodeStatus sets status of the given nodeURL
func (np *NodePool) SetNodeStatus(url *url.URL, status bool) {
	for _, n := range np.nodes {
		if n.URL.String() == url.String() {
			n.SetProps(status)
			break
		}
	}
}

// GetAttemptsFromContext returns the attempts for request
func GetAttemptsFromContext(r *http.Request) int {
	if attempts, ok := r.Context().Value(Attempts).(int); ok {
		return attempts
	}
	return 1
}

// GetRetryFromContext returns the retry for request
func GetRetryFromContext(r *http.Request) int {
	if retry, ok := r.Context().Value(Retry).(int); ok {
		return retry
	}
	return 0
}

// Check health of nodes periodically
func healthCheck() {
	t := time.NewTicker(time.Minute * 2)
	for {
		select {
		case <-t.C:
			log.Printf("Starting health check...")
			nodePool.HealthCheck()

		}
	}
}

var nodePool NodePool

func main() {
	var nodeList string
	var port int
	flag.StringVar(&nodeList, "nodeList", "", "List of avaiable nodes comma-separated")
	flag.IntVar(&port, "port", 3030, "Port to serve load-balancer")
	flag.Parse()

	if len(nodeList) == 0 {
		log.Fatal("Please provide one or more nodes to load balance")
	}

	for _, nodeURL := range strings.Split(nodeList, ",") {
		nodeURLParsed, err := url.Parse(nodeURL)
		if err != nil {
			log.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(nodeURLParsed)
		proxy.ErrorHandler = func(w http.ResponseWriter, request *http.Request, e error) {
			log.Printf("[%s] %s\n", nodeURLParsed.Host, e.Error())
			retries := GetRetryFromContext(request)
			if retries < 3 {
				select {
				case <-time.After(10 * time.Millisecond):
					ctx := context.WithValue(request.Context(), Retry, retries+1)
					proxy.ServeHTTP(w, request.WithContext(ctx))
				}
				return
			}

			// Try diff node
			attempts := GetAttemptsFromContext(request)
			log.Printf("%s(%s) Attempting retry %d\n", request.RemoteAddr, request.URL.Path, attempts)
			ctx := context.WithValue(request.Context(), Attempts, attempts+1)


			// After 3 retries, set this node as dead
			if attempts >= 3 {
				nodePool.SetNodeStatus(nodeURLParsed, false)
			}

			loadBalancer(w, request.WithContext(ctx))

		}
		nodePool.AddNode(&Node{
			URL:          nodeURLParsed,
			Active:       true,
			weight:       1,
			ReverseProxy: proxy,
		})

		log.Printf("Configured node: %s\n", nodeURLParsed)
	}

	// Create LB server
	server := http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: http.HandlerFunc(loadBalancer),
	}

	go healthCheck()

	log.Printf("Load Balancer started on port: %d", port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
