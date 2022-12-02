package sftps

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/textproto"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Ftp struct {
	rawConn  net.Conn
	tlsConn  *tls.Conn
	ctrlConn *textproto.Conn
	params   *ftpParameters
	State    int
}

func newFtp(p *ftpParameters) (ftp *Ftp) {
	//spew.Dump(p)

	ftp = new(Ftp)
	ftp.params = p
	ftp.State = OFFLINE
	return
}

func (this *Ftp) connect() (res *FtpResponse, err error) {
	var ipaddr []net.IP
	var code int
	var msg string

	if ipaddr, err = net.LookupIP(this.params.host); err != nil {
		return
	}

	addr := fmt.Sprintf("%s:%d", ipaddr[0], this.params.port)

	dialer := new(net.Dialer)
	if dialer.Timeout, err = time.ParseDuration(TIMEOUT); err != nil {
		return
	}
	if dialer.KeepAlive, err = time.ParseDuration(KEEPALIVE); err != nil {
		return
	}
	if this.params.secure && this.params.secureMode == IMPLICIT {
		var conf *tls.Config
		if conf, err = this.getTLSConfig(); err != nil {
			return
		}
		if this.tlsConn, err = tls.DialWithDialer(dialer, "tcp", addr, conf); err != nil {
			return
		}
		this.ctrlConn = textproto.NewConn(this.tlsConn)
	} else {
		if this.rawConn, err = dialer.Dial("tcp", addr); err != nil {
			return
		}
		this.ctrlConn = textproto.NewConn(this.rawConn)
	}
	if code, msg, err = this.ctrlConn.ReadResponse(220); err != nil {
		return
	}

	res = &FtpResponse{
		command: "",
		code:    code,
		msg:     msg,
	}

	this.State = ONLINE
	return
}

func (this *Ftp) secureUpgrade() (err error) {
	var conf *tls.Config
	if conf, err = this.getTLSConfig(); err != nil {
		return
	}
	this.tlsConn = tls.Client(this.rawConn, conf)
	this.ctrlConn = textproto.NewConn(this.tlsConn)
	return
}

func (this *Ftp) getTLSConfig() (conf *tls.Config, err error) {
	var certPair tls.Certificate
	var certPool *x509.CertPool
	var rcaPem []byte

	conf = new(tls.Config)
	conf.ClientAuth = tls.VerifyClientCertIfGiven
	conf.CipherSuites = []uint16{
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	}

	if this.params.cert != "" && this.params.key != "" {
		if certPair, err = tls.LoadX509KeyPair(this.params.cert, this.params.key); err != nil {
			return
		}

		certPool = x509.NewCertPool()

		if this.params.rootCA != "" {
			if rcaPem, err = ioutil.ReadFile("./cert/rcaPem.pem"); err != nil {
				return
			}

			if this.params.alwaysTrust {
				if !certPool.AppendCertsFromPEM(rcaPem) {
					panic("Failed to parse the Root Certificate")
				}
			}
			conf.RootCAs = certPool
		}

		conf.Certificates = make([]tls.Certificate, 1)
		conf.Certificates[0] = certPair
		conf.ClientCAs = certPool
	}
	conf.InsecureSkipVerify = this.params.alwaysTrust
	return
}

func (this *Ftp) auth() (res []*FtpResponse, err error) {

	var r *FtpResponse

	res = []*FtpResponse{}

	if this.params.secure && this.params.secureMode == EXPLICIT {
		if r, err = this.Command("AUTH TLS", 234); err != nil {
			return
		}
		res = append(res, r)

		if err = this.secureUpgrade(); err != nil {
			return
		}
	}

	if r, err = this.Command(fmt.Sprintf("USER %s", this.params.user), 331); err != nil {
		return
	}
	res = append(res, r)

	if r, err = this.Command(fmt.Sprintf("PASS %s", this.params.pass), 230); err != nil {
		return
	}
	res = append(res, r)

	return
}

func (this *Ftp) Command(cmd string, code int) (res *FtpResponse, err error) {
	var c int
	var m string

	if _, err = this.ctrlConn.Cmd(cmd); err != nil {
		return
	}

	if c, m, err = this.ctrlConn.ReadResponse(code); err != nil {
		return
	}

	res = &FtpResponse{
		command: cmd,
		code:    c,
		msg:     m,
	}
	return
}

func (this *Ftp) options() (res []*FtpResponse, err error) {
	var r *FtpResponse
	if r, err = this.Command("SYST", 215); err != nil {
		return
	}
	res = []*FtpResponse{}
	res = append(res, r)

	if r, err = this.Command("FEAT", 211); err != nil {
		return
	}
	res = append(res, r)

	if r, err = this.Command("OPTS UTF8 ON", 200); err != nil {
		return
	}
	res = append(res, r)

	if this.params.secure {
		if r, err = this.Command("PROT P", 200); err != nil {
			return
		}
		res = append(res, r)
	}
	if r, err = this.Command("TYPE I", 200); err != nil {
		return
	}
	res = append(res, r)
	return
}

func (this *Ftp) getLocalIP() (ip string, err error) {
	var addrs []net.Addr
	if addrs, err = net.InterfaceAddrs(); err != nil {
		return
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				ip = ipnet.IP.To4().String()
			}
		}
	}
	if ip == "" {
		err = errors.New("Could not get the Local Address.")
		return
	}
	return
}

func (this *Ftp) ds2h(dec string) (hex string, err error) {
	var p int = 0
	if p, err = strconv.Atoi(dec); err != nil {
		return
	}
	hex = fmt.Sprintf("%x", p)
	return
}

func (this *Ftp) h2i(hex string) (res int, err error) {
	if r, e := strconv.ParseInt(hex, 16, 64); e != nil {
		return
	} else {
		res = int(r)
	}
	return
}

func (this *Ftp) getSplitPorts() (port1 int, port2 int, err error) {
	hex := fmt.Sprintf("%x", this.params.listenPort)

	switch len(hex) {
	case 1, 2:
		port1 = 0
		if port2, err = this.h2i(hex); err != nil {
			return
		}
	case 3:
		p1 := hex[:1]
		p2 := hex[1:3]
		if port1, err = this.h2i(p1); err != nil {
			return
		}
		if port2, err = this.h2i(p2); err != nil {
			return
		}
	case 4:
		p1 := hex[:2]
		p2 := hex[2:4]
		if port1, err = this.h2i(p1); err != nil {
			return
		}
		if port2, err = this.h2i(p2); err != nil {
			return
		}
	default:
		err = errors.New("The Port Number could not converted to the Parameter Format for the Listen Function.")
	}
	return
}

func (this *Ftp) port() (res *FtpResponse, listener net.Listener, err error) {
	var localIP string = ""
	if localIP, err = this.getLocalIP(); err != nil {
		return
	}

	ip := strings.Replace(localIP, ".", ",", -1)
	var p1, p2 int
	if p1, p2, err = this.getSplitPorts(); err != nil {
		return
	}
	cmd := fmt.Sprintf("PORT %s,%d,%d", ip, p1, p2)

	if res, err = this.Command(cmd, 200); err != nil {
		return
	}
	if listener, err = net.Listen("tcp", fmt.Sprintf("%s:%d", localIP, this.params.listenPort)); err != nil {
		return
	}

	return
}

func (this *Ftp) pasv() (res *FtpResponse, dataConn net.Conn, err error) {
	if res, err = this.Command("PASV", 227); err != nil {
		return
	}
	var ip []net.IP
	if ip, err = net.LookupIP(this.params.host); err != nil {
		return
	}
	reg := regexp.MustCompile("([0-9]+?),([0-9]+?),([0-9]+?),([0-9]+?),([0-9]+?),([0-9]+)")
	matches := reg.FindAllStringSubmatch(res.msg, -1)
	tmp := matches[0]

	var hex1 string = ""
	var hex2 string = ""
	if hex1, err = this.ds2h(tmp[5]); err != nil {
		return
	}
	if hex2, err = this.ds2h(tmp[6]); err != nil {
		return
	}
	var port int
	if port, err = this.h2i(fmt.Sprintf("%s%s", hex1, hex2)); err != nil {
		return
	}
	param := fmt.Sprintf("%s:%d", ip[0], port)

	dataConn, err = net.Dial("tcp", param)
	return
}

func (this *Ftp) readBytes(itf interface{}) (res *FtpResponse, bytes []byte, err error) {
	var dataConn net.Conn

	if this.params.passive {
		if dc, ok := itf.(net.Conn); ok {
			dataConn = dc
		} else {
			err = errors.New("Invalid parameter were bound, net.Conn is not found.")
			return
		}

	} else {
		if listener, ok := itf.(net.Listener); ok {
			if dataConn, err = listener.Accept(); err != nil {
				return
			}
		} else {
			err = errors.New("Invalid parameter were bound, net.Listener is not found.")
			return
		}
	}

	defer dataConn.Close()
	if this.params.secure {
		var conf *tls.Config
		if conf, err = this.getTLSConfig(); err != nil {
			return
		}
		dataTLS := tls.Client(dataConn, conf)
		defer dataTLS.Close()

		if bytes, err = ioutil.ReadAll(dataTLS); err != nil {
			return
		}
		dataTLS.Close() // Important the Buffer flush out.

	} else {
		if bytes, err = ioutil.ReadAll(dataConn); err != nil {
			return
		}
		dataConn.Close() // Important the Buffer flush out.
	}

	c, m, e := this.ctrlConn.ReadResponse(226)
	if e != nil {
		err = e
		return
	}
	res = &FtpResponse{
		command: "",
		code:    c,
		msg:     m,
	}
	return
}

func (this *Ftp) quit() (res *FtpResponse, err error) {

	defer this.ctrlConn.Close()

	if this.params.secure {
		defer this.tlsConn.Close()
	}
	if this.params.secureMode != IMPLICIT {
		defer this.rawConn.Close()
	}

	if res, err = this.Command("QUIT", 221); err != nil {
		return
	}
	return
}

func (this *Ftp) list(p string) (res []*FtpResponse, list string, err error) {
	res = []*FtpResponse{}

	if !this.params.keepAlive {
		defer func() {
			var r *FtpResponse
			if r, err = this.quit(); err != nil {
				return
			}
			res = append(res, r)
			return
		}()
	}

	var itf interface{}
	var bytes []byte
	var r *FtpResponse
	res = []*FtpResponse{}

	cmd := fmt.Sprintf("LIST -aL %s", p)

	if this.params.passive {
		if r, itf, err = this.pasv(); err != nil {
			return
		}
	} else {
		if r, itf, err = this.port(); err != nil {
			return
		}
	}
	res = append(res, r)

	if r, err = this.Command(cmd, 150); err != nil {
		return
	}
	res = append(res, r)

	if r, bytes, err = this.readBytes(itf); err != nil {
		return
	}
	res = append(res, r)

	list = string(bytes)

	return
}

func (this *Ftp) download(local string, remote string) (res []*FtpResponse, len int64, err error) {
	res = []*FtpResponse{}
	if !this.params.keepAlive {
		defer func() {
			var r *FtpResponse
			if r, err = this.quit(); err != nil {
				return
			}
			res = append(res, r)
			return
		}()
	}

	var itf interface{}
	var r *FtpResponse

	if this.params.passive {
		if r, itf, err = this.pasv(); err != nil {
			return
		}
	} else {
		if r, itf, err = this.port(); err != nil {
			return
		}
	}
	res = append(res, r)
	var cmd = fmt.Sprintf("RETR %s", remote)
	if r, err = this.Command(cmd, 150); err != nil {

		return
	}
	res = append(res, r)

	if r, len, err = this.fileTransfer(DOWNLOAD, local, itf); err != nil {
		return
	}
	res = append(res, r)
	return
}

func (this *Ftp) upload(local string, remote string) (res []*FtpResponse, len int64, err error) {
	res = []*FtpResponse{}
	if !this.params.keepAlive {
		defer func() {
			var r *FtpResponse
			if r, err = this.quit(); err != nil {
				return
			}
			res = append(res, r)
			return
		}()
	}

	var itf interface{}
	var r *FtpResponse

	if this.params.passive {
		if r, itf, err = this.pasv(); err != nil {
			return
		}
	} else {
		if r, itf, err = this.port(); err != nil {
			return
		}
	}
	res = append(res, r)
	var cmd = fmt.Sprintf("STOR %s", remote)
	if r, err = this.Command(cmd, 150); err != nil {
		return
	}
	res = append(res, r)

	if r, len, err = this.fileTransfer(UPLOAD, local, itf); err != nil {
		return
	}
	res = append(res, r)
	return
}

func (this *Ftp) mkdir(p string) (res *FtpResponse, err error) {
	res, err = this.Command(fmt.Sprintf("MKD %s", p), 257)
	return
}

func (this *Ftp) rmdir(p string) (res *FtpResponse, err error) {
	res, err = this.Command(fmt.Sprintf("RMD %s", p), 250)
	return
}

func (this *Ftp) delete(p string) (res *FtpResponse, err error) {
	res, err = this.Command(fmt.Sprintf("DELE %s", p), 200)
	return
}

func (this *Ftp) rename(old, new string) (res []*FtpResponse, err error) {
	r := &FtpResponse{}
	if r, err = this.Command(fmt.Sprintf("RNFR %s", old), 350); err != nil {
		return
	}
	res = append(res, r)
	if r, err = this.Command(fmt.Sprintf("RNTO %s", new), 250); err != nil {
		return
	}
	res = append(res, r)
	return
}

func (this *Ftp) fileTransfer(direction int, uri string, itf interface{}) (res *FtpResponse, len int64, err error) {

	var dataConn net.Conn

	if this.params.passive {
		if c, ok := itf.(net.Conn); ok {
			dataConn = c
			defer dataConn.Close()
		} else {
			err = errors.New("Invalid parameter were bound, Value of the argument 'itf' must be the Type 'net.Conn' when the Passive Mode specified by the Parameter.")
			return
		}
	} else {
		if listener, ok := itf.(net.Listener); ok {
			defer listener.Close()
			if dataConn, err = listener.Accept(); err != nil {
				return
			}
		} else {
			err = errors.New("Invalid parameter were bound, Value of the argument 'itf' must be the Type 'net.Listener' whern the Active Mode speciffied by the Parameter")
			return
		}
	}

	var r io.ReadCloser
	var w io.WriteCloser
	var rw io.ReadWriteCloser = dataConn

	if this.params.secure {
		var conf *tls.Config
		if conf, err = this.getTLSConfig(); err != nil {
			return
		}
		dataTLS := tls.Client(dataConn, conf)
		defer dataTLS.Close()
		rw = dataTLS
	}

	if direction == DOWNLOAD {
		if w, err = os.Create(uri); err != nil {
			return
		}
		r = rw
	} else if direction == UPLOAD {

		if r, err = os.Open(uri); err != nil {
			return
		}
		w = rw
	} else {
		err = errors.New("The Argument 'direction' must be the either 'DOWNLOAD' or 'UPLOAD'.")
		return
	}

	if len, err = io.Copy(w, r); err != nil {
		return
	}
	r.Close()
	w.Close()

	var code int
	var msg string
	if code, msg, err = this.ctrlConn.ReadResponse(226); err != nil {
		return
	}
	res = &FtpResponse{
		command: "",
		code:    code,
		msg:     msg,
	}
	return
}
