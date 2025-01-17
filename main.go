// Copyright 2018 Jack Henry and Associates
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// kube-inress-index is a service which watches kubernetes namespaces
// and builds an index.html type page linking to each Ingress.
//
// It's meant to be used as a "table of contents".
//
// The code was inspired and based off of the following projects
// https://github.com/kubernetes/client-go/tree/master/examples
// https://github.com/jetstack/kube-lego
package main

import (
	"errors"
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"context"

	k8sNetworking "k8s.io/api/networking/v1"
	k8sMeta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	// Import OIDC provider -- https://github.com/coreos/tectonic-forum/issues/99
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
)

var (
	// flags
	flagAddress             = flag.String("address", "0.0.0.0:8080", "Address to listen on")
	flagForceTLS            = flag.Bool("force-tls", true, "Force all URLs to be HTTPS, even if their Ingress objects has no TLS object")
	flagKubeconfig          *string
	flagWatchableNamespaces = flag.String("namespaces", "", "Namespaces to watch (required)")

	// default settings
	resyncInterval = 60 * time.Second

	ctx context.Context = context.Background()
)

func main() {
	if home := homeDir(); home != "" {
		flagKubeconfig = flag.String("kubeconfig", filepath.Join(homeDir(), ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		flagKubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	// validation
	var watchableNamespaces = strings.Split(*flagWatchableNamespaces, ",")
	if *flagWatchableNamespaces == "" {
		ns := os.Getenv("NAMESPACES")
		flagWatchableNamespaces = &ns
	}
	if *flagWatchableNamespaces == "" || len(watchableNamespaces) == 0 {
		panic("You need to specify -namespaces for namespaces to watch")
	}
	sort.Strings(watchableNamespaces)
	fmt.Printf("watching namespaces: %s\n", strings.Join(watchableNamespaces, ", "))

	// try and get config from cluster
	config, err := rest.InClusterConfig()
	if err != nil {
		// read config from -kubeconfig flag
		config, err = clientcmd.BuildConfigFromFlags("", *flagKubeconfig)
		if err != nil {
			panic(fmt.Sprintf("error reading config, err=%v", err))
		}
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(fmt.Sprintf("error setting up Kubernetes API client, err=%v", err))
	}

	// ingress
	respChan := make(chan []ingress, 10)
	go watchIngresses(clientset, watchableNamespaces, respChan)

	// catch signals
	signalChan := make(chan os.Signal, 1)
	doneChan := make(chan error, 1)
	go handleSignals(signalChan, doneChan)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	// setup http page
	listenHTTP(*flagAddress, respChan, doneChan)
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

func handleSignals(signalChan chan os.Signal, doneChan chan error) {
	for s := range signalChan {
		doneChan <- fmt.Errorf("shutdown initiated, signal=%v", s)
		return
	}
}

var pageContent = `<!doctype html>
<html>
  <head>
    <title>kube-ingress-index</title>
    <meta name="viewport" content="width=device-width, initial-scale=1">
  </head>
  <body>
    <h2>kube-ingress-index</h2>
    <ul>
      {{range $ing := .Ingresses}}
        <li>{{ $ing.Namespace }} / <a href="{{ $ing.FQDN }}">{{ $ing.Name }}</a></li>
      {{else}}
      <li>No Ingress objects found</li>
      {{end}}
    </ul>
  </body>
</html>`

func listenHTTP(address string, respChan chan []ingress, doneChan chan error) {
	var curIngresses []ingress

	srv := &http.Server{
		Addr: address,
	}

	go func() {
		for {
			select {
			case err := <-doneChan:
				fmt.Println(err.Error())
				srv.Shutdown(nil)
				return

			case cur := <-respChan:
				curIngresses = cur
				sortIngresses(curIngresses)
			}
		}
	}()

	tpl := template.Must(template.New("contents").Parse(pageContent))
	handler := func(w http.ResponseWriter, r *http.Request) {
		err := tpl.Execute(w, struct {
			Ingresses []ingress
		}{
			Ingresses: curIngresses,
		})
		if err != nil {
			http.Error(w, "500 internal server error", http.StatusInternalServerError)
		}
	}

	fmt.Printf("listening on %s\n", address)
	http.HandleFunc("/", handler)
	srv.ListenAndServe()
}

func sortIngresses(ing []ingress) {
	sort.Slice(ing, func(i, j int) bool {
		return strings.ToLower(ing[i].String()) < strings.ToLower(ing[j].String())
	})
}

func ingressListFunc(c *kubernetes.Clientset, ns string) func(k8sMeta.ListOptions) (runtime.Object, error) {
	return func(opts k8sMeta.ListOptions) (runtime.Object, error) {
		return c.NetworkingV1().Ingresses(ns).List(ctx, opts)
	}
}

func ingressWatchFunc(c *kubernetes.Clientset, ns string) func(options k8sMeta.ListOptions) (watch.Interface, error) {
	return func(options k8sMeta.ListOptions) (watch.Interface, error) {
		return c.NetworkingV1().Ingresses(ns).Watch(ctx, options)
	}
}

// TODO(adam): return multiple FQDN's
func buildFQDN(ing *k8sNetworking.Ingress) string {
	tlsHosts := make(map[string]bool)
	spec := ing.Spec
	for i := range spec.TLS {
		for j := range spec.TLS[i].Hosts {
			tlsHosts[spec.TLS[i].Hosts[j]] = true
		}
	}

	for i := range spec.Rules {
		host := spec.Rules[i].Host

		var u *url.URL
		if *flagForceTLS || tlsHosts[host] {
			u, _ = url.Parse(fmt.Sprintf("https://%s", host))
		} else {
			u, _ = url.Parse(fmt.Sprintf("http://%s", host))
		}
		if u == nil || u.Host == "" || strings.HasPrefix(u.Host, "localhost:") { // ignore invalid rules/hosts
			continue
		}

		return u.String()
	}
	return ""
}

func buildIngress(ing *k8sNetworking.Ingress) (*ingress, error) {
	fqdn := buildFQDN(ing)
	if fqdn == "" {
		return nil, errors.New("empty FQDN")
	}
	return &ingress{
		Namespace: ing.Namespace,
		Name:      ing.Name,
		FQDN:      fqdn,
	}, nil
}

// ingress is a smaller model for internal shipping about
type ingress struct {
	Name, Namespace string

	// FQDN is an address which the backend is reachable from
	FQDN string
}

func (ing ingress) String() string {
	return fmt.Sprintf("Ingress: namespace=%s, name=%s, fqdn=%s", ing.Namespace, ing.Name, ing.FQDN)
}

type ingresses struct {
	// current set of Ingress objects
	active []ingress
	mu     sync.Mutex
}

func (i *ingresses) upsert(ing ingress) []ingress {
	i.mu.Lock()
	defer i.mu.Unlock()

	found := false
	for k := range i.active {
		if i.active[k].Name == ing.Name {
			found = true
			break // we've already added this ingress
		}
	}
	if !found { // didn't find our ingress, add it and return
		i.active = append(i.active, ing)
	}

	// return a copy
	out := make([]ingress, len(i.active))
	copy(out, i.active)
	return out
}

func (i *ingresses) delete(ing ingress) []ingress {
	i.mu.Lock()
	defer i.mu.Unlock()

	var next []ingress
	for k := range i.active {
		if i.active[k].Name == ing.Name {
			continue
		}
		next = append(next, i.active[k])
	}
	i.active = next

	// return a copy
	out := make([]ingress, len(i.active))
	copy(out, i.active)
	return out
}

func watchIngresses(kubeClient *kubernetes.Clientset, namespaces []string, respChan chan []ingress) {
	// Internal accumulator, a copy is sent back each time
	accum := &ingresses{}

	ingEventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			addIng, ok := obj.(*k8sNetworking.Ingress)
			if ok {
				ing, err := buildIngress(addIng)
				if err == nil {
					current := accum.upsert(*ing)
					respChan <- current
					fmt.Printf("added %s, watching %d Ingress objects\n", ing.String(), len(current))
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			delIng, ok := obj.(*k8sNetworking.Ingress)
			if ok {
				ing, err := buildIngress(delIng)
				if err == nil {
					current := accum.delete(*ing)
					respChan <- current
					fmt.Printf("deleted %s, watching %d Ingress objects\n", ing.String(), len(current))
				}
			}
		},
		UpdateFunc: func(_, cur interface{}) {
			upIng, ok := cur.(*k8sNetworking.Ingress)
			if ok {
				ing, err := buildIngress(upIng)
				if err == nil {
					current := accum.upsert(*ing)
					respChan <- current
					fmt.Printf("updated %s, watching %d Ingress objects\n", ing.String(), len(current))
				}
			}
		},
	}

	for i := range namespaces {
		watch := &cache.ListWatch{
			ListFunc:  ingressListFunc(kubeClient, namespaces[i]),
			WatchFunc: ingressWatchFunc(kubeClient, namespaces[i]),
		}
		_, controller := cache.NewInformer(watch, &k8sNetworking.Ingress{}, resyncInterval, ingEventHandler)
		go controller.Run(nil) // TODO(adam): pass doneChan through to here
	}
}
