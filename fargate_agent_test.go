// +build fargate_integration

package instana_test

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	instana "github.com/instana/go-sensor"
	"github.com/instana/testify/assert"
	"github.com/instana/testify/require"
)

var agent *serverlessAgent

func TestMain(m *testing.M) {
	teardownEnv := setupAWSFargateEnv()
	defer teardownEnv()

	teardownSrv := setupMetadataServer()
	defer teardownSrv()

	defer restoreEnvVarFunc("INSTANA_AGENT_KEY")
	os.Setenv("INSTANA_AGENT_KEY", "testkey1")

	var err error
	agent, err = setupServerlessAgent()
	if err != nil {
		log.Fatalf("failed to initialize serverless agent: %s", err)
	}

	instana.InitSensor(&instana.Options{})

	os.Exit(m.Run())
}

func TestFargateAgent_SendMetrics(t *testing.T) {
	defer agent.Reset()

	require.Eventually(t, func() bool { return len(agent.Metrics) > 0 }, 2*time.Second, 500*time.Millisecond)

	collected := agent.Metrics[0]

	assert.Equal(t, "arn:aws:ecs:us-east-2:012345678910:task/9781c248-0edd-4cdb-9a93-f63cb662a5d3::nginx-curl", collected.Header.Get("X-Instana-Host"))
	assert.Equal(t, "testkey1", collected.Header.Get("X-Instana-Key"))
	assert.NotEmpty(t, collected.Header.Get("X-Instana-Time"))

	var payload struct {
		Plugins []struct {
			Name     string                 `json:"name"`
			EntityID string                 `json:"entityId"`
			Data     map[string]interface{} `json:"data"`
		} `json:"plugins"`
	}
	require.NoError(t, json.Unmarshal(collected.Body, &payload))

	pluginData := make(map[string][]serverlessAgentPluginPayload)
	for _, plugin := range payload.Plugins {
		pluginData[plugin.Name] = append(pluginData[plugin.Name], serverlessAgentPluginPayload{plugin.EntityID, plugin.Data})
	}

	t.Run("AWS ECS Task plugin payload", func(t *testing.T) {
		require.Len(t, pluginData["com.instana.plugin.aws.ecs.task"], 1)
		d := pluginData["com.instana.plugin.aws.ecs.task"][0]

		assert.NotEmpty(t, d.EntityID)
		assert.Equal(t, d.Data["taskArn"], d.EntityID)

		assert.Equal(t, "default", d.Data["clusterArn"])
		assert.Equal(t, "nginx", d.Data["taskDefinition"])
		assert.Equal(t, "5", d.Data["taskDefinitionVersion"])
	})

	t.Run("AWS ECS Container plugin payload", func(t *testing.T) {
		require.Len(t, pluginData["com.instana.plugin.aws.ecs.container"], 2)

		containers := make(map[string]serverlessAgentPluginPayload)
		for _, container := range pluginData["com.instana.plugin.aws.ecs.container"] {
			containers[container.EntityID] = container
		}

		t.Run("instrumented", func(t *testing.T) {
			d := containers["arn:aws:ecs:us-east-2:012345678910:task/9781c248-0edd-4cdb-9a93-f63cb662a5d3::nginx-curl"]
			require.NotEmpty(t, d)

			assert.NotEmpty(t, d.EntityID)

			require.IsType(t, d.Data["taskArn"], "")
			require.IsType(t, d.Data["containerName"], "")
			assert.Equal(t, d.Data["taskArn"].(string)+"::"+d.Data["containerName"].(string), d.EntityID)

			if assert.NotEmpty(t, d.Data["taskArn"]) {
				require.NotEmpty(t, pluginData["com.instana.plugin.aws.ecs.task"])
				assert.Equal(t, pluginData["com.instana.plugin.aws.ecs.task"][0].EntityID, d.Data["taskArn"])
			}

			assert.Equal(t, true, d.Data["instrumented"])
			assert.Equal(t, "go", d.Data["runtime"])
			assert.Equal(t, "43481a6ce4842eec8fe72fc28500c6b52edcc0917f105b83379f88cac1ff3946", d.Data["dockerId"])
		})

		t.Run("non-instrumented", func(t *testing.T) {
			d := containers["arn:aws:ecs:us-east-2:012345678910:task/9781c248-0edd-4cdb-9a93-f63cb662a5d3::~internal~ecs~pause"]
			require.NotEmpty(t, d)

			assert.NotEmpty(t, d.EntityID)

			require.IsType(t, d.Data["taskArn"], "")
			require.IsType(t, d.Data["containerName"], "")
			assert.Equal(t, d.Data["taskArn"].(string)+"::"+d.Data["containerName"].(string), d.EntityID)

			if assert.NotEmpty(t, d.Data["taskArn"]) {
				require.NotEmpty(t, pluginData["com.instana.plugin.aws.ecs.task"])
				assert.Equal(t, pluginData["com.instana.plugin.aws.ecs.task"][0].EntityID, d.Data["taskArn"])
			}

			assert.Nil(t, d.Data["instrumented"])
			assert.Empty(t, d.Data["runtime"])
			assert.Equal(t, "731a0d6a3b4210e2448339bc7015aaa79bfe4fa256384f4102db86ef94cbbc4c", d.Data["dockerId"])
		})
	})

	t.Run("Docker plugin payload", func(t *testing.T) {
		require.Len(t, pluginData["com.instana.plugin.docker"], 1)
		d := pluginData["com.instana.plugin.docker"][0]

		assert.NotEmpty(t, d.EntityID)
		assert.Equal(t, "43481a6ce4842eec8fe72fc28500c6b52edcc0917f105b83379f88cac1ff3946", d.Data["Id"])

		var found bool
		for _, container := range pluginData["com.instana.plugin.aws.ecs.container"] {
			if container.Data["containerName"] == "nginx-curl" {
				found = true
				assert.Equal(t, container.EntityID, d.EntityID)
				break
			}
		}
		assert.True(t, found)
	})

	t.Run("Process plugin payload", func(t *testing.T) {
		require.Len(t, pluginData["com.instana.plugin.process"], 1)
		d := pluginData["com.instana.plugin.process"][0]

		assert.NotEmpty(t, d.EntityID)

		assert.Equal(t, "docker", d.Data["containerType"])
		assert.Equal(t, "43481a6ce4842eec8fe72fc28500c6b52edcc0917f105b83379f88cac1ff3946", d.Data["container"])
	})

	t.Run("Go process plugin payload", func(t *testing.T) {
		require.Len(t, pluginData["com.instana.plugin.golang"], 1)
		d := pluginData["com.instana.plugin.golang"][0]

		assert.NotEmpty(t, d.EntityID)

		require.NotEmpty(t, pluginData["com.instana.plugin.process"])
		assert.Equal(t, pluginData["com.instana.plugin.process"][0].EntityID, d.EntityID)

		assert.NotEmpty(t, d.Data["metrics"])
	})
}

func TestFargateAgent_SendSpans(t *testing.T) {
	defer agent.Reset()

	sensor := instana.NewSensor("testing")

	sp := sensor.Tracer().StartSpan("entry")
	sp.SetTag("value", "42")
	sp.Finish()

	require.Eventually(t, func() bool { return len(agent.Traces) > 0 }, 2*time.Second, 500*time.Millisecond)

	collected := agent.Traces[0]

	assert.Equal(t, "arn:aws:ecs:us-east-2:012345678910:task/9781c248-0edd-4cdb-9a93-f63cb662a5d3::nginx-curl", collected.Header.Get("X-Instana-Host"))
	assert.Equal(t, "testkey1", collected.Header.Get("X-Instana-Key"))
	assert.NotEmpty(t, collected.Header.Get("X-Instana-Time"))

	var spans []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(collected.Body, &spans))

	require.Len(t, spans, 1)
	assert.JSONEq(t, `{"hl": true, "cp": "aws", "e": "arn:aws:ecs:us-east-2:012345678910:task/9781c248-0edd-4cdb-9a93-f63cb662a5d3::nginx-curl"}`, string(spans[0]["f"]))
}

func setupAWSFargateEnv() func() {
	teardown := restoreEnvVarFunc("AWS_EXECUTION_ENV")
	os.Setenv("AWS_EXECUTION_ENV", "AWS_ECS_FARGATE")

	return teardown
}

func setupMetadataServer() func() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		http.ServeFile(w, req, "aws/testdata/container_metadata.json")
	})
	mux.HandleFunc("/task", func(w http.ResponseWriter, req *http.Request) {
		http.ServeFile(w, req, "aws/testdata/task_metadata.json")
	})

	srv := httptest.NewServer(mux)

	teardown := restoreEnvVarFunc("ECS_CONTAINER_METADATA_URI")
	os.Setenv("ECS_CONTAINER_METADATA_URI", srv.URL)

	return func() {
		teardown()
		srv.Close()
	}
}

type serverlessAgentPluginPayload struct {
	EntityID string
	Data     map[string]interface{}
}

type serverlessAgentRequest struct {
	Header http.Header
	Body   []byte
}

type serverlessAgent struct {
	Metrics []serverlessAgentRequest
	Traces  []serverlessAgentRequest

	ln           net.Listener
	restoreEnvFn func()
}

func setupServerlessAgent() (*serverlessAgent, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize the serverless agent listener: %s", err)
	}

	srv := &serverlessAgent{
		ln:           ln,
		restoreEnvFn: restoreEnvVarFunc("INSTANA_ENDPOINT_URL"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", srv.HandleMetrics)
	mux.HandleFunc("/traces", srv.HandleTraces)

	go http.Serve(ln, mux)

	os.Setenv("INSTANA_ENDPOINT_URL", "http://"+ln.Addr().String())

	return srv, nil
}

func (srv *serverlessAgent) HandleMetrics(w http.ResponseWriter, req *http.Request) {
	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		log.Printf("ERROR: failed to read serverless agent metrics request body: %s", err)
		body = nil
	}

	srv.Metrics = append(srv.Metrics, serverlessAgentRequest{
		Header: req.Header,
		Body:   body,
	})

	w.WriteHeader(http.StatusNoContent)
}

func (srv *serverlessAgent) HandleTraces(w http.ResponseWriter, req *http.Request) {
	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		log.Printf("ERROR: failed to read serverless agent spans request body: %s", err)
		body = nil
	}

	srv.Traces = append(srv.Traces, serverlessAgentRequest{
		Header: req.Header,
		Body:   body,
	})

	w.WriteHeader(http.StatusNoContent)
}

func (srv *serverlessAgent) Reset() {
	srv.Metrics = nil
	srv.Traces = nil
}

func (srv *serverlessAgent) Teardown() {
	srv.restoreEnvFn()
	srv.ln.Close()
}

func restoreEnvVarFunc(key string) func() {
	if oldValue, ok := os.LookupEnv(key); ok {
		return func() { os.Setenv(key, oldValue) }
	}

	return func() { os.Unsetenv(key) }
}
