package main

import (
	"fmt"
	"github.com/smartcontractkit/chainlink-env/environment"
	"github.com/smartcontractkit/chainlink-env/pkg"
	"github.com/smartcontractkit/chainlink-env/pkg/helm/chainlink"
	"github.com/smartcontractkit/chainlink-env/pkg/helm/ethereum"
)

func main() {
	// example of quick usage to debug env, removed on SIGINT
	//os.Setenv("CHAINLINK_IMAGE", "ddd")
	//os.Setenv("CHAINLINK_VERSION", "aaa")
	err := environment.New(&environment.Config{
		Labels:            []string{fmt.Sprintf("envType=%s", pkg.EnvTypeEVM5)},
		KeepConnection:    true,
		RemoveOnInterrupt: true,
	}).
		//AddHelm(mockservercfg.New(nil)).
		//AddHelm(mockserver.New(nil)).
		AddHelm(ethereum.New(nil)).
		AddHelm(chainlink.New(0, map[string]interface{}{
			"chainlink": map[string]interface{}{
				"resources": map[string]interface{}{
					"requests": map[string]interface{}{
						"cpu": "344m",
					},
					"limits": map[string]interface{}{
						"cpu": "344m",
					},
				},
			},
			"db": map[string]interface{}{
				"stateful": "true",
				"capacity": "5Gi",
			},
		})).
		AddHelm(chainlink.New(1,
			map[string]interface{}{
				"chainlink": map[string]interface{}{
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{
							"cpu": "577m",
						},
						"limits": map[string]interface{}{
							"cpu": "577m",
						},
					},
				},
			})).
		Run()
	if err != nil {
		panic(err)
	}
}
