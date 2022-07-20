package main

import (
	"bytes"
	"fmt"
	"github.com/gorilla/mux"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"proxy/balancer"
	"proxy/config"
	"proxy/middleware"
	"proxy/util"
	"proxy/util/logging"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ReverseProxy = "Balancer-Reverse-Proxy"
)

//Proxy 路由代理
type Proxy struct {
	mux sync.RWMutex
	//bl 通过请求时的url，获取具体的负载均衡器
	bl balancer.Balancer
	//alive 主机存活检测
	alive map[string]bool
	//reverseProxyMap 根据负载均衡器返回的host，获取对应的反向代理
	reverseProxyMap map[string]*httputil.ReverseProxy
}

//NewRouterHandler 创建处理器
func NewRouterHandler(cfg *config.Config) (*mux.Router,error) {
	muxRouter := mux.NewRouter()
	muxRouter.Use(middleware.PanicsHandling)
	if cfg.MaxAllowed > 0 {
		muxRouter.Use(middleware.MaxAllowedMiddleware(cfg.MaxAllowed))
	}

	for _, r := range cfg.Routes {
		if err := cfg.ValidationAlgorithm(r.Algorithm); err != nil {
			return nil, err
		}

		upstreamPath := r.UpstreamPathParse()
		downstreamPath := r.DownstreamPathParse()
		proxyRoute, err := NewProxy(r.Algorithm, r.DownstreamScheme, upstreamPath, downstreamPath, r.DownstreamHostAndPorts)
		if err != nil {
			return nil, err
		}

		if cfg.HealthCheck {
			proxyRoute.HealthCheck(cfg.HealthCheckInterval)
		}

		muxRouter.PathPrefix(upstreamPath).Handler(proxyRoute).Methods(r.UpstreamHTTPMethod...)
	}
	return muxRouter,nil
}

//NewProxy 接收下游的主机信息，返回下游主机代理
func NewProxy(algorithm string,scheme string,upstreamPath string,downstreamPath string, downstreamHosts []config.DownstreamHost) (*Proxy,error) {
	var targetHosts []string
	alive := make(map[string]bool)
	reverseProxyMap := make(map[string]*httputil.ReverseProxy)

	for _, dsh := range downstreamHosts {
		host, err := dsh.GetDownstreamHost(scheme)
		if err != nil {
			return nil, err
		}
		alive[host] = true
		targetHosts = append(targetHosts, host)
		reverseProxyMap[host] = newSingleHostReverseProxy(scheme, host, upstreamPath, downstreamPath)

		logging.Infof("主机 %s 正常，已添加到负载均衡器", host)
	}
	lb, err := balancer.Build(algorithm, targetHosts)
	if err != nil {
		return nil, err
	}

	proxy := &Proxy{
		bl:              lb,
		alive:           alive,
		reverseProxyMap: reverseProxyMap,
	}
	return proxy, nil
}


//ServeHTTP 实现到http服务器的代理
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := fmt.Sprintf("%s?%s", r.URL.Path, r.URL.RawQuery)
	host, err := p.bl.Balance(key)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		errStr := fmt.Sprintf("负载均衡器: %s", err.Error())
		logging.Error(errStr)
		_, _ = w.Write([]byte(errStr))
		return
	}
	p.bl.Inc(host)
	defer p.bl.Done(host)
	p.reverseProxyMap[host].ServeHTTP(w, r)
}

//HealthCheck 主机健康检查
func (p *Proxy) HealthCheck(interval uint) {
	for host := range p.reverseProxyMap {
		go p.healthCheck(host, interval)
	}
}

func (p *Proxy) healthCheck(host string, interval uint) {
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	for range ticker.C {
		isBackendAlive := util.IsBackendAlive(host)
		if !isBackendAlive && p.ReadAlive(host) {
			logging.Errorf("主机 %s 不可用，已从负载均衡器中移除", host)

			p.SetAlive(host, false)
			p.bl.Remove(host)
		} else if isBackendAlive && !p.ReadAlive(host) {
			logging.Errorf("主机 %s 恢复正常，已添加到负载均衡器", host)

			p.SetAlive(host, true)
			p.bl.Add(host)
		}
	}
}

// ReadAlive 获取主机存活状态
func (p *Proxy) ReadAlive(url string) bool {
	p.mux.RLock()
	defer p.mux.RUnlock()
	return p.alive[url]
}

// SetAlive 设置主机存活状态
func (p *Proxy) SetAlive(url string, alive bool) {
	p.mux.Lock()
	defer p.mux.Unlock()
	p.alive[url] = alive
}

var transport = &http.Transport{
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second, //连接超时
		KeepAlive: 30 * time.Second, //长连接超时时间
	}).DialContext,
	MaxIdleConns:          100,              //最大空闲连接
	IdleConnTimeout:       90 * time.Second, //空闲超时时间
	TLSHandshakeTimeout:   10 * time.Second, //tls握手超时时间
	ExpectContinueTimeout: 1 * time.Second,  //100-continue 超时时间
}

func newSingleHostReverseProxy(scheme string,host string,upstreamPath string,downstreamPath string)*httputil.ReverseProxy {
	director := func(req *http.Request) {
		req.URL.Host = host
		req.URL.Scheme = scheme

		targetPath := strings.Replace(req.URL.Path, upstreamPath, downstreamPath, 1)
		req.URL.Path = targetPath

		if _, ok := req.Header["User-Agent"]; !ok {
			req.Header.Set("User-Agent", "user-agent")
		}
		req.Header.Set(util.XProxy, ReverseProxy)
		req.Header.Set(util.XRealIP, util.GetIP(req))
	}

	//更改内容
	modifyFunc := func(resp *http.Response) error {
		if resp.StatusCode != 200 {
			//获取内容
			oldPayload, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			//追加内容
			newPayload := []byte("StatusCode error:" + string(oldPayload))
			resp.Body = ioutil.NopCloser(bytes.NewBuffer(newPayload))
			resp.ContentLength = int64(len(newPayload))
			resp.Header.Set("Content-Length", strconv.FormatInt(int64(len(newPayload)), 10))
		}
		return nil
	}

	//错误回调 ：关闭real_server时测试，错误回调
	errorHandler := func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "ErrorHandler error:"+err.Error(), 500)
	}

	return &httputil.ReverseProxy{
		Director:       director,
		Transport:      transport,
		ModifyResponse: modifyFunc,
		ErrorHandler:   errorHandler,
	}
}

//func (s *Server) RegisterHost(w http.ResponseWriter, r *http.Request)  {
//	_ = r.ParseForm()
//	host := r.Form["host"][0]
//
//
//	err := p.RegisterHost(r.Form["host"][0])
//	if err != nil {
//		w.WriteHeader(http.StatusInternalServerError)
//		_, _ = fmt.Fprintf(w, err.Error())
//		return
//	}
//
//	_, _ = fmt.Fprintf(w, fmt.Sprintf("register host: %s success", r.Form["host"][0]))
//}



//func unregisterHost(w http.ResponseWriter, r *http.Request) {
//	_ = r.ParseForm()
//
//	err := p.UnregisterHost(r.Form["host"][0])
//	if err != nil {
//		w.WriteHeader(http.StatusInternalServerError)
//		_, _ = fmt.Fprintf(w, err.Error())
//		return
//	}
//
//	_, _ = fmt.Fprintf(w, fmt.Sprintf("unregister host: %s success", r.Form["host"][0]))
//}
//
//func getKey(w http.ResponseWriter, r *http.Request) {
//	_ = r.ParseForm()
//
//	val, err := p.GetKey(r.Form["key"][0])
//	if err != nil {
//		w.WriteHeader(http.StatusInternalServerError)
//		_, _ = fmt.Fprintf(w, err.Error())
//		return
//	}
//
//	_, _ = fmt.Fprintf(w, fmt.Sprintf("key: %s, val: %s", r.Form["key"][0], val))
//}
//
//func getKeyLeast(w http.ResponseWriter, r *http.Request) {
//	_ = r.ParseForm()
//
//	val, err := p.GetKeyLeast(r.Form["key"][0])
//	if err != nil {
//		w.WriteHeader(http.StatusInternalServerError)
//		_, _ = fmt.Fprintf(w, err.Error())
//		return
//	}
//
//	_, _ = fmt.Fprintf(w, fmt.Sprintf("key: %s, val: %s", r.Form["key"][0], val))
//}