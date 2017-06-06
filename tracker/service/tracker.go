package service

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"code.uber.internal/go-common.git/x/log"
	"code.uber.internal/infra/kraken/config/tracker"
	"code.uber.internal/infra/kraken/tracker/peerhandoutpolicy"
	"code.uber.internal/infra/kraken/tracker/storage"

	"code.uber.internal/infra/kraken/utils"

	bencode "github.com/jackpal/bencode-go"
	"github.com/pressly/chi"
	"github.com/uber-common/bark"
)

// WebApp defines a web-app that is backed by a cache.Cache
type webApp interface {
	HealthHandler(http.ResponseWriter, *http.Request)
	GetAnnounceHandler(http.ResponseWriter, *http.Request)
	GetInfoHashHandler(http.ResponseWriter, *http.Request)
	PostInfoHashHandler(w http.ResponseWriter, r *http.Request)
	GetManifestHandler(http.ResponseWriter, *http.Request)
	PostManifestHandler(w http.ResponseWriter, r *http.Request)
}

type webAppStruct struct {
	appCfg    config.AppConfig
	datastore storage.Storage
	policy    peerhandoutpolicy.PeerHandoutPolicy
}

// AnnouncerResponse follows a bittorrent tracker protocol
// for tracker based peer discovery
type AnnouncerResponse struct {
	Interval int64              `bencode:"interval"`
	Peers    []storage.PeerInfo `bencode:"peers"`
}

// newWebApp instantiates a web-app API backed by the input cache
func newWebApp(cfg config.AppConfig, storage storage.Storage) webApp {
	policy, ok := peerhandoutpolicy.Get(cfg.PeerHandoutPolicy.Priority, cfg.PeerHandoutPolicy.Sampling)
	if !ok {
		log.Fatalf(
			"Peer handout policy not found: priority=%s sampling=%s",
			cfg.PeerHandoutPolicy.Priority, cfg.PeerHandoutPolicy.Sampling)
	}
	return &webAppStruct{appCfg: cfg, datastore: storage, policy: policy}
}

// formatRequest generates ascii representation of a request
func (webApp *webAppStruct) FormatRequest(r *http.Request) string {
	// Create return string
	var request []string
	// Add the request string
	url := fmt.Sprintf("%v %v %v", r.Method, r.URL, r.Proto)
	request = append(request, url)
	// Add the host
	request = append(request, fmt.Sprintf("Host: %v", r.Host))
	// Loop through headers
	for name, headers := range r.Header {
		name = strings.ToLower(name)
		for _, h := range headers {
			request = append(request, fmt.Sprintf("%v: %v", name, h))
		}
	}

	// If this is a POST, add post data
	if r.Method == "POST" {
		r.ParseForm()
		request = append(request, "\n")
		request = append(request, r.Form.Encode())
	}
	// Return the request as a string
	return strings.Join(request, "\n")
}

func (webApp *webAppStruct) GetAnnounceHandler(w http.ResponseWriter, r *http.Request) {
	log.Debugf("Received announce requet from: %s", r.Host)

	queryValues := r.URL.Query()

	infoHash := hex.EncodeToString([]byte(queryValues.Get("info_hash")))
	peerID := hex.EncodeToString([]byte(queryValues.Get("peer_id")))
	peerPortStr := queryValues.Get("port")
	peerIPStr := queryValues.Get("ip")
	peerDC := queryValues.Get("dc")
	peerBytesDownloadedStr := queryValues.Get("downloaded")
	peerBytesUploadedStr := queryValues.Get("uploaded")
	peerBytesLeftStr := queryValues.Get("left")
	peerEvent := queryValues.Get("event")

	peerPort, err := strconv.ParseInt(peerPortStr, 10, 64)
	if err != nil {
		log.Infof("Port is not parsable: %s", webApp.FormatRequest(r))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	peerIPInt32, err := strconv.ParseInt(peerIPStr, 10, 32)
	if err != nil {
		log.Infof("Peer's ip address is not a valid integer: %s", webApp.FormatRequest(r))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	peerBytesDownloaded, err := strconv.ParseInt(peerBytesDownloadedStr, 10, 64)
	if err != nil {
		log.Infof("Downloaded is not parsable: %s", webApp.FormatRequest(r))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	peerBytesUploaded, err := strconv.ParseInt(peerBytesUploadedStr, 10, 64)
	if err != nil {
		log.Infof("Uploaded is not parsable: %s", webApp.FormatRequest(r))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	peerBytesLeft, err := strconv.ParseUint(peerBytesLeftStr, 10, 64)
	if err != nil {
		log.Infof("left is not parsable: %s", webApp.FormatRequest(r))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	peerIP := utils.Int32toIP(int32(peerIPInt32)).String()

	peer := &storage.PeerInfo{
		InfoHash:        infoHash,
		PeerID:          peerID,
		IP:              peerIP,
		Port:            peerPort,
		DC:              peerDC,
		BytesUploaded:   peerBytesUploaded,
		BytesDownloaded: peerBytesDownloaded,
		// TODO (@evelynl): our torrent library use uint64 as bytes left but database/sql does not support it
		BytesLeft: int64(peerBytesLeft),
		Event:     peerEvent,
	}

	err = webApp.datastore.Update(peer)
	if err != nil {
		log.Infof("Could not update storage for: hash %s, error: %s, request: %s",
			infoHash, err.Error(), webApp.FormatRequest(r))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	peerInfos, err := webApp.datastore.Read(infoHash)
	if err != nil {
		log.Infof("Could not read storage: hash %s, error: %s, request: %s",
			infoHash, err.Error(), webApp.FormatRequest(r))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = webApp.policy.AssignPeerPriority(peerIP, peerDC, peerInfos)
	if err != nil {
		log.Infof("Could not apply a peer handout priority policy: %s, error : %s, request: %s",
			infoHash, err.Error(), webApp.FormatRequest(r))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// TODO(codyg): Accept peer limit query argument.
	peerInfos, err = webApp.policy.SamplePeers(peerInfos, len(peerInfos))
	if err != nil {
		msg := "Could not apply peer handout sampling policy"
		log.WithFields(log.Fields{
			"error":     err.Error(),
			"info_hash": infoHash,
			"request":   webApp.FormatRequest(r),
		}).Info(msg)
		http.Error(w, fmt.Sprintf("%s: %v", msg, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// TODO(codyg): bencode can't serialize pointers, so we're forced to dereference
	// every PeerInfo first.
	derefPeerInfos := make([]storage.PeerInfo, len(peerInfos))
	for i, p := range peerInfos {
		derefPeerInfos[i] = *p
	}

	// write peers bencoded
	err = bencode.Marshal(w, AnnouncerResponse{
		Interval: webApp.appCfg.Announcer.AnnounceInterval,
		Peers:    derefPeerInfos,
	})
	if err != nil {
		log.Infof("Bencode marshalling has failed: %s for request: %s", err.Error(), webApp.FormatRequest(r))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (webApp *webAppStruct) HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("OK ;-)\n"))
}

func (webApp *webAppStruct) GetInfoHashHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	queryValues := r.URL.Query()

	name := queryValues.Get("name")
	if name == "" {
		log.Errorf("Failed to get torrent info hash, no name specified: %s", webApp.FormatRequest(r))
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Failed to get torrent info hash: no torrent name specified"))
		return
	}

	info, err := webApp.datastore.ReadTorrent(name)
	if err != nil {
		log.Errorf("Failed to get torrent info hash: %s", webApp.FormatRequest(r))
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Failed to get torrent info hash: %s", err.Error())))
		log.WithFields(bark.Fields{
			"name":  name,
			"error": err,
		}).Error("Failed to get torrent info hash")
		return
	}

	if info == nil {
		log.Infof("Torrent info hash is not found: %s", webApp.FormatRequest(r))

		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf("Failed to get torrent info hash: name %s not found", name)))
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(info.InfoHash))
	log.Infof("Successfully got infohash for %s: %s", name, info.InfoHash)
}

func (webApp *webAppStruct) PostInfoHashHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	queryValues := r.URL.Query()

	name := queryValues.Get("name")
	infohash := queryValues.Get("info_hash")
	if name == "" || infohash == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Failed to create torrent: incomplete query"))
		return
	}

	err := webApp.datastore.CreateTorrent(
		&storage.TorrentInfo{
			TorrentName: name,
			InfoHash:    infohash,
		},
	)

	if err != nil {
		log.Errorf("Failed to creat torrent: %s", webApp.FormatRequest(r))

		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Failed to create torrent: %s", err.Error())))
		log.WithFields(bark.Fields{
			"name":  name,
			"error": err,
		}).Error("Failed to get torrent info hash")
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Created"))
}

func (webApp *webAppStruct) GetManifestHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	name := chi.URLParam(r, "name")
	if len(name) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Failed to parse an empty tag name"))
		return
	}

	name, err := url.QueryUnescape(name)
	if err != nil {
		log.Errorf("Cannot unescape name: %s", webApp.FormatRequest(r))

		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(
			fmt.Sprintf("cannot unescape manifest name: %s, error: %s",
				name, err.Error())))
		log.WithFields(
			bark.Fields{"name": name, "error": err}).Error(
			"Failed to unescape manifest name")
		return
	}

	manifest, err := webApp.datastore.ReadManifest(name)
	if err != nil {
		log.Errorf("Cannot read manifest: %s", webApp.FormatRequest(r))

		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(
			fmt.Sprintf("cannot unescape manifest name: %s, error: %s",
				name, err.Error())))
		log.WithFields(
			bark.Fields{"name": name, "error": err}).Error(
			"Failed to unescape manifest name")
		return
	}

	if manifest == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.Write([]byte(manifest.Manifest))
	w.WriteHeader(http.StatusOK)
	log.Infof("Got manifest for %s", name)
}

func (webApp *webAppStruct) PostManifestHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	name := chi.URLParam(r, "name")

	if len(name) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Failed to parse a tag name"))
		return
	}

	name, err := url.QueryUnescape(name)
	if err != nil {
		log.Errorf("Cannot unescape manifest name: %s", webApp.FormatRequest(r))

		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(
			fmt.Sprintf("cannot unescape manifest name: %s, error: %s",
				name, err.Error())))
		log.WithFields(
			bark.Fields{"name": name, "error": err}).Error(
			"Failed to unescape manifest name")
		return
	}

	var jsonManifest map[string]interface{}
	manifest, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Errorf("Cannot read post request: %s", webApp.FormatRequest(r))

		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(
			fmt.Sprintf("Could not read manifest from a post payload for %s and error: %s",
				name, err.Error())))
		log.WithFields(
			bark.Fields{"name": name, "error": err}).Error(
			"Failed to read manifest payload")
		return
	}

	err = json.Unmarshal(manifest, &jsonManifest)
	defer r.Body.Close()

	if err != nil {
		log.Errorf("Cannot unmarshal manifest: %s", webApp.FormatRequest(r))

		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(
			fmt.Sprintf("Mnifest is an invalid json for %s, manifest %s and error: %s",
				name, manifest[:], err.Error())))
		log.WithFields(
			bark.Fields{"name": name, "manifest": manifest[:], "error": err}).Error(
			"Failed to parse manifest")
		return
	}

	err = webApp.datastore.UpdateManifest(
		&storage.Manifest{TagName: name, Manifest: string(manifest[:]), Flags: 0})
	if err != nil {
		log.Errorf("Cannot update the manifest: %s", webApp.FormatRequest(r))

		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(
			fmt.Sprintf("Failed to update manifest for %s with manifest %s and error: %s",
				name, manifest, err.Error())))
		log.WithFields(
			bark.Fields{"name": name, "manifest": manifest[:], "error": err}).Error(
			"Failed to update manifest")
		return
	}

	w.WriteHeader(http.StatusOK)
	log.Infof("Updated manifest successfully for %s", name)
}