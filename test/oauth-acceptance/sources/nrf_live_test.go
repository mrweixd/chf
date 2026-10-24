package sbi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	nrfctx "github.com/free5gc/nrf/internal/context"
	"github.com/free5gc/nrf/internal/sbi/processor"
	"github.com/free5gc/nrf/pkg/factory"
	"github.com/free5gc/openapi/models"
	"github.com/free5gc/openapi/oauth"
	"github.com/free5gc/util/mongoapi"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

type liveNRFApp struct {
	ServerNrf
	cfg  *factory.Config
	proc *processor.Processor
}

func (a *liveNRFApp) Config() *factory.Config         { return a.cfg }
func (a *liveNRFApp) Context() *nrfctx.NRFContext     { return nrfctx.GetSelf() }
func (a *liveNRFApp) Processor() *processor.Processor { return a.proc }

func TestLiveOAuthAcceptance(t *testing.T) {
	mongod := os.Getenv("OAUTH_MONGOD")
	if mongod == "" {
		t.Skip("set OAUTH_MONGOD and OAUTH_BIN_DIR")
	}
	dir := t.TempDir()
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(dir, "mongo.log"))
			t.Log(string(b))
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	dbDir := filepath.Join(dir, "db")
	require.NoError(t, os.Mkdir(dbDir, 0700))
	cmd := exec.Command(mongod, "--bind_ip", "127.0.0.1", "--port", fmt.Sprint(port), "--dbpath", dbDir, "--quiet", "--logpath", filepath.Join(dir, "mongo.log"))
	require.NoError(t, cmd.Start())
	defer func() { _ = cmd.Process.Signal(os.Interrupt); _ = cmd.Wait() }()
	mongoURI := fmt.Sprintf("mongodb://127.0.0.1:%d", port)
	dbName := "oauth_acceptance_" + uuid.NewString()
	require.NoError(t, mongoapi.SetMongoDB(dbName, mongoURI))
	defer mongoapi.Client.Disconnect(context.Background())
	require.Eventually(t, func() bool {
		c, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return mongoapi.Client.Ping(c, readpref.Primary()) == nil
	}, 20*time.Second, 100*time.Millisecond)
	rootKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	root, err := oauth.GenerateRootCertificate(filepath.Join(dir, "root.pem"), rootKey)
	require.NoError(t, err)
	nrfKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	nrfID := uuid.NewString()
	certPath := filepath.Join(dir, "nrf.pem")
	nrfCert, err := oauth.GenerateCertificate("NRF", nrfID, certPath, &nrfKey.PublicKey, root, rootKey)
	require.NoError(t, err)
	self := nrfctx.GetSelf()
	self.NrfNfProfile = models.Nrf_NFMgmt_NFProfile{NfInstanceId: nrfID, NfType: models.Nrf_NFMgmt_NFType_NRF, NfStatus: models.Nrf_NFMgmt_NFStatus_REGISTERED}
	self.NrfPrivKey = nrfKey
	self.NrfCert = nrfCert
	self.RootCert = root
	ids := map[string]string{}
	for _, kind := range []string{"CHF", "PCF", "NEF", "SMF", "SMSF", "AMF", "AF", "UDR", "AUSF"} {
		id := uuid.NewString()
		ids[kind] = id
		_, err = oauth.GenerateCertificate(kind, id, oauth.GetNFCertPath(dir, kind, id), &nrfKey.PublicKey, root, rootKey)
		require.NoError(t, err)
		_, err = mongoapi.Client.Database(dbName).Collection(nrfctx.NfProfileCollName).InsertOne(context.Background(), bson.M{"nfInstanceId": id, "nfType": kind, "nfStatus": "REGISTERED"})
		require.NoError(t, err)
	}
	cfg := &factory.Config{Configuration: &factory.Configuration{NfInstanceId: nrfID, Sbi: &factory.Sbi{Scheme: "http", BindingIPv4: "127.0.0.1", OAuth: true, NfCertBasePath: dir}, ServiceNameList: []string{"nnrf-nfm", "nnrf-disc"}}}
	factory.NrfConfig = cfg
	app := &liveNRFApp{cfg: cfg, proc: &processor.Processor{}}
	server, err := NewServer(app, "")
	require.NoError(t, err)
	httpServer := httptest.NewUnstartedServer(server.router)
	httpServer.Config.Protocols = new(http.Protocols)
	httpServer.Config.Protocols.SetHTTP1(true)
	httpServer.Config.Protocols.SetUnencryptedHTTP2(true)
	httpServer.Start()
	defer httpServer.Close()
	idsJSON, err := json.Marshal(ids)
	require.NoError(t, err)
	for _, nf := range []string{"chf", "pcf", "nef"} {
		t.Run(nf, func(t *testing.T) {
			child := exec.Command(filepath.Join(os.Getenv("OAUTH_BIN_DIR"), nf+".test"), "-test.run=^TestLiveOAuthAcceptance$", "-test.v", "-test.timeout=90s")
			child.Env = append(os.Environ(), "OAUTH_NRF_URL="+httpServer.URL, "OAUTH_NRF_CERT="+certPath, "OAUTH_NRF_ID="+nrfID, "OAUTH_IDS="+string(idsJSON), "OAUTH_MONGO_URI="+mongoURI, "OAUTH_DB="+dbName)
			output, err := child.CombinedOutput()
			t.Log(string(output))
			require.NoError(t, err)
		})
	}
}
