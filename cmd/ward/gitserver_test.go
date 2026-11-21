package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// serveGit answers the smart HTTP protocol of git in the test process, on the bare repository of the remote, so a fetch
// or a push of the mirror starts the one process of its own git client and no server process. Fetch speaks protocol
// version 2, as git does by default; push speaks version 0, as git does.
func (r *remote) serveGit(w http.ResponseWriter, req *http.Request) {
	sto := filesystem.NewStorage(osfs.New(r.dir), cache.NewObjectLRUDefault())
	svc := path.Base(req.URL.Path)
	advertise := strings.HasSuffix(req.URL.Path, "/info/refs")
	if advertise {
		svc = req.URL.Query().Get("service")
	}
	v2 := strings.Contains(req.Header.Get("Git-Protocol"), "version=2")
	var err error
	switch {
	case advertise && svc == "git-upload-pack" && v2:
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, err = io.WriteString(w, pkt("version 2\n")+pkt("agent=test\n")+pkt("ls-refs\n")+pkt("fetch\n")+pkt("object-format=sha1\n")+"0000")
	case advertise && svc == "git-receive-pack":
		err = r.advertiseReceive(w, sto)
	case svc == "git-upload-pack" && v2:
		err = r.uploadPack(w, req, sto)
	case svc == "git-receive-pack" && !advertise:
		err = r.receivePack(w, req, sto)
	default:
		http.NotFound(w, req)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

func (r *remote) advertiseReceive(w http.ResponseWriter, sto *filesystem.Storage) error {
	s, err := server.NewServer(fixedLoader{sto}).NewReceivePackSession(nil, nil)
	if err != nil {
		return err
	}
	ar, err := s.AdvertisedReferences()
	if err != nil {
		return err
	}
	ar.Prefix = [][]byte{[]byte("# service=git-receive-pack"), pktline.Flush}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	return ar.Encode(w)
}

// readPkts returns the data pkt-lines of a v2 request and marks each delim pkt with a nil line.
func readPkts(body io.Reader) ([][]byte, error) {
	br := bufio.NewReader(body)
	var out [][]byte
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if err == io.EOF {
				return out, nil
			}
			return nil, err
		}
		n, err := strconv.ParseUint(string(hdr[:]), 16, 32)
		if err != nil {
			return nil, err
		}
		if n < 4 {
			if n == 1 {
				out = append(out, nil)
			}
			continue
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(br, line); err != nil {
			return nil, err
		}
		out = append(out, bytes.TrimSuffix(line, []byte("\n")))
	}
}

func (r *remote) uploadPack(w http.ResponseWriter, req *http.Request, sto *filesystem.Storage) error {
	body := io.Reader(req.Body)
	if req.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(req.Body)
		if err != nil {
			return err
		}
		body = zr
	}
	lines, err := readPkts(body)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	switch {
	case len(lines) > 0 && string(lines[0]) == "command=ls-refs":
		return lsRefs(w, sto, lines)
	case len(lines) > 0 && string(lines[0]) == "command=fetch":
		return fetchPack(w, sto, lines)
	}
	return fmt.Errorf("unsupported command %q", lines)
}

func lsRefs(w io.Writer, sto *filesystem.Storage, lines [][]byte) error {
	var prefixes []string
	peel := false
	for _, l := range lines {
		if p, ok := strings.CutPrefix(string(l), "ref-prefix "); ok {
			prefixes = append(prefixes, p)
		}
		peel = peel || string(l) == "peel"
	}
	var out strings.Builder
	iter, err := sto.IterReferences()
	if err != nil {
		return err
	}
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().String()
		if ref.Type() != plumbing.HashReference || !hasPrefix(name, prefixes) {
			return nil
		}
		line := ref.Hash().String() + " " + name
		if peeled := peelTag(sto, ref.Hash()); peel && peeled != ref.Hash() {
			line += " peeled:" + peeled.String()
		}
		out.WriteString(pkt(line + "\n"))
		return nil
	})
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, out.String()+"0000")
	return err
}

func hasPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return len(prefixes) == 0
}

func peelTag(sto storer.EncodedObjectStorer, h plumbing.Hash) plumbing.Hash {
	for {
		t, err := object.GetTag(sto, h)
		if err != nil {
			return h
		}
		h = t.Target
	}
}

// fetchPack answers a fetch command. It is ready as soon as it holds one of the haves, so a fetch takes one round.
func fetchPack(w io.Writer, sto *filesystem.Storage, lines [][]byte) error {
	var wants, haves []plumbing.Hash
	done := false
	for _, l := range lines {
		switch s := string(l); {
		case strings.HasPrefix(s, "want "):
			wants = append(wants, plumbing.NewHash(strings.TrimPrefix(s, "want ")))
		case strings.HasPrefix(s, "have "):
			if h := plumbing.NewHash(strings.TrimPrefix(s, "have ")); sto.HasEncodedObject(h) == nil {
				haves = append(haves, h)
			}
		case s == "done":
			done = true
		}
	}
	var out bytes.Buffer
	if !done {
		out.WriteString(pkt("acknowledgments\n"))
		if len(haves) == 0 {
			out.WriteString(pkt("NAK\n") + "0000")
			_, err := w.Write(out.Bytes())
			return err
		}
		for _, h := range haves {
			out.WriteString(pkt("ACK " + h.String() + "\n"))
		}
		out.WriteString(pkt("ready\n") + "0001")
	}
	for _, h := range wants {
		if err := sto.HasEncodedObject(h); err != nil {
			return fmt.Errorf("not our ref %s", h)
		}
	}
	skip, err := revlist.Objects(sto, haves, nil)
	if err != nil {
		return err
	}
	objs, err := revlist.Objects(sto, wants, skip)
	if err != nil {
		return err
	}
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, sto, false).Encode(objs, 0); err != nil {
		return err
	}
	out.WriteString(pkt("packfile\n"))
	for data := pack.Bytes(); len(data) > 0; {
		n := min(len(data), 65515)
		out.WriteString(pkt("\x01" + string(data[:n])))
		data = data[n:]
	}
	out.WriteString("0000")
	_, err = w.Write(out.Bytes())
	return err
}

// receivePack applies a push. A push that names a refused ref fails whole, as a pre-receive hook that exits 1 makes it fail.
func (r *remote) receivePack(w http.ResponseWriter, req *http.Request, sto *filesystem.Storage) error {
	body := io.Reader(req.Body)
	if req.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(req.Body)
		if err != nil {
			return err
		}
		body = zr
	}
	ur := packp.NewReferenceUpdateRequest()
	if err := ur.Decode(body); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	refused, deletesOnly := false, true
	for _, c := range ur.Commands {
		deletesOnly = deletesOnly && c.Action() == packp.Delete
		if r.logging {
			r.pushes = append(r.pushes, fmt.Sprintf("%s %s %s\n", c.Old, c.New, c.Name))
		}
		refused = refused || r.refuse[c.Name.String()]
	}
	var rs *packp.ReportStatus
	if refused {
		rs = packp.NewReportStatus()
		rs.UnpackStatus = "ok"
		for _, c := range ur.Commands {
			rs.CommandStatuses = append(rs.CommandStatuses, &packp.CommandStatus{ReferenceName: c.Name, Status: "pre-receive hook declined"})
		}
	} else {
		s, err := server.NewServer(fixedLoader{sto}).NewReceivePackSession(nil, nil)
		if err != nil {
			return err
		}
		if deletesOnly {
			ur.Packfile = nil
		}
		rs, _ = s.ReceivePack(req.Context(), ur)
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	if rs == nil {
		return nil
	}
	return rs.Encode(w)
}

type fixedLoader struct{ storer.Storer }

func (l fixedLoader) Load(*transport.Endpoint) (storer.Storer, error) { return l.Storer, nil }
