package service

import (
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"repospanner.org/repospanner/server/storage"
	"repospanner.org/repospanner/server/utils"

	"github.com/sirupsen/logrus"
	"repospanner.org/repospanner/server/constants"
)

func (cfg *Service) addSecurityHeaders(w http.ResponseWriter) {
	// Add totally paranoid security headers for $redacted
	w.Header()["Strict-Transport-Security"] = []string{"max-age=31536000; includeSubDomains; preload"}
	w.Header()["X-Frame-Options"] = []string{"DENY"}
	w.Header()["X-Xss-Protection"] = []string{"1; mode=block"}
	w.Header()["X-Content-Type-Options"] = []string{"nosniff"}
	w.Header()["Referrer-Policy"] = []string{"no-referrer"}
	w.Header()["Content-Security-Policy"] = []string{"default-src 'none'"}
	w.Header()["Cache-Control"] = []string{"no-cache"}

	// Add our identity
	w.Header()["X-Repospanner-NodeName"] = []string{cfg.nodename}
}

func (cfg *Service) prereq(w http.ResponseWriter, r *http.Request, server string) (*logrus.Entry, permissionInfo) {
	cfg.addSecurityHeaders(w)

	reqlogger := cfg.log.WithFields(logrus.Fields{
		"server":   server,
		"protocol": r.Proto,
		"method":   r.Method,
		"client":   r.RemoteAddr,
		"url":      r.URL,
	})
	hdr, ok := r.Header["User-Agent"]
	if ok && len(hdr) >= 1 {
		reqlogger = reqlogger.WithField(
			"useragent", hdr[0],
		)
	}
	reqlogger = resolveCompression(r, reqlogger)

	var perminfo permissionInfo
	if r.TLS != nil {
		perminfo = cfg.getPermissionInfo(r.TLS)
		reqlogger = cfg.addPermToLogger(perminfo, reqlogger)
	}

	return reqlogger, perminfo
}

func findProjectAndOp(parts []string) (string, string) {
	var reponame string
	var command string
	for i, part := range parts {
		if strings.HasSuffix(part, ".git") {
			reponame = strings.Join(parts[:i+1], "/")
			command = strings.Join(parts[i+1:], "/")
			break
		}
	}
	if reponame != "" {
		reponame = reponame[:len(reponame)-4]
	}
	return reponame, command
}

func resolveCompression(r *http.Request, reqlogger *logrus.Entry) *logrus.Entry {
	// If the request indicated it's gzip-compressd, set r.Body to a
	// gzip.Reader
	encoding := r.Header.Get("Content-Encoding")
	reqlogger = reqlogger.WithField("encoding", encoding)
	if encoding == "" {
		return reqlogger
	} else if encoding == "gzip" {
		rdr, err := gzip.NewReader(r.Body)
		if err != nil {
			panic(err)
		}
		r.Body = utils.NewInnerReadCloser(rdr, r.Body, true)
		return reqlogger
	}
	panic(fmt.Sprintf("Invalid content type: '%s'", encoding))
}

func (cfg *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqlogger, perminfo := cfg.prereq(w, r, "gitservice")

	repobridge := r.Header[http.CanonicalHeaderKey("X-RepoBridge-Version")]
	if len(repobridge) != 0 {
		reqlogger = reqlogger.WithField(
			"RepoBridge-Version", repobridge,
		)
	}

	if r.TLS == nil {
		reqlogger.Info("Non-TLS request received")
		w.WriteHeader(401)
		w.Write([]byte("TLS Required"))
		return
	}
	pathparts := strings.Split(path.Clean(r.URL.Path), "/")[1:]

	if len(pathparts) == 1 && pathparts[0] == "" {
		reqlogger.Debug("Root page requested")

		w.WriteHeader(200)
		w.Write([]byte("Hello " + perminfo.Username + ", welcome to repospanner " + constants.PublicVersionString() + "\n"))
		return
	}
	if pathparts[0] == "repo" {
		// Repo handling here
		pathparts = pathparts[1:]
		reponame, command := findProjectAndOp(pathparts)
		if reponame == "" || command == "" {
			reqlogger.Debug("Repo URL requested without repo or command")
			http.NotFound(w, r)
			return
		}

		reqlogger = reqlogger.WithFields(logrus.Fields{
			"reponame": reponame,
			"command":  command,
		})

		if !cfg.statestore.hasRepo(reponame) {
			reqlogger.Debug("Non-existing repo requested")
			http.NotFound(w, r)
			return
		}

		if !cfg.checkAccess(perminfo, reponame, constants.CertPermissionRead) {
			// If we don't have read access, we have no access to this repo
			reqlogger.Info("Unauthorized request")
			http.NotFound(w, r)
			return
		}

		if command == "info/refs" {
			cfg.serveGitDiscovery(w, r, perminfo, reqlogger, reponame, false)
			return
		} else if command == "git-upload-pack" {
			cfg.serveGitUploadPack(w, r, reqlogger, reponame)
			return
		} else if command == "git-receive-pack" {
			if !cfg.checkAccess(perminfo, reponame, constants.CertPermissionWrite) {
				// Write access denied
				reqlogger.Info("Unauthorized request")
				http.NotFound(w, r)
				return
			}

			cfg.serveGitReceivePack(w, r, reqlogger, reponame)
			return
		} else if command == "simple/refs" {
			w.WriteHeader(200)
			for refname, refval := range cfg.statestore.getGitRefs(reponame) {
				fmt.Fprintf(w, "real\x00%s\x00%s\x00\n", refname, refval)
			}
			for refname, refval := range cfg.statestore.getSymRefs(reponame) {
				fmt.Fprintf(w, "symb\x00%s\x00%s\x00\n", refname, refval)
			}
			return
		} else if strings.HasPrefix(command, "simple/object/") {
			objectids := command[len("simple/object/"):]
			if !isValidRef(objectids) {
				http.NotFound(w, r)
				return
			}
			objectid := storage.ObjectID(objectids)
			projdriver := cfg.gitstore.GetProjectStorage(reponame)
			objtype, objsize, reader, err := projdriver.ReadObject(objectid)
			if err == storage.ErrObjectNotFound {
				http.NotFound(w, r)
				return
			} else if err != nil {
				http.Error(w, "Error retrieving object", 500)
				return
			}

			w.WriteHeader(200)
			zwriter := zlib.NewWriter(w)
			fmt.Fprintf(zwriter, "%s %d\x00", objtype.HdrName(), objsize)
			if _, err = io.Copy(zwriter, reader); err != nil {
				reqlogger.Error("Error writing object", err)
				http.Error(w, "Error writing", 500)
				return
			}
			if err = zwriter.Close(); err != nil {
				reqlogger.Error("Error writing object", err)
				http.Error(w, "Error writing", 500)
				return
			}
			return
		}
		reqlogger.Debug("Unknown command requested")
		http.NotFound(w, r)
		return
	} else if pathparts[0] == "admin" {
		if !cfg.checkAccess(perminfo, "*", constants.CertPermissionAdmin) {
			reqlogger.Info("Unauthorized admin request received")

			w.WriteHeader(401)
			w.Write([]byte("No authorized for admin access\n"))
			return
		}
		if pathparts[1] == "nodestatus" {
			cfg.serveAdminNodeStatus(w, r)
			return
		} else if pathparts[1] == "createrepo" {
			cfg.serveAdminCreateRepo(w, r)
			return
		} else if pathparts[1] == "editrepo" {
			cfg.serveAdminEditRepo(w, r)
			return
		} else if pathparts[1] == "listrepos" {
			cfg.serveAdminListRepos(w, r)
			return
		} else if pathparts[1] == "deleterepo" {
			cfg.serveAdminDeleteRepo(w, r)
			return
		} else if pathparts[1] == "hook" {
			cfg.serveAdminHooksMgmt(w, r)
			return
		}
	}

	reqlogger.Debug("Unknown page requested")
	http.NotFound(w, r)
	return
}
