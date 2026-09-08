// Exercise the production budget tree using explicit synthetic usage counters.
package main
import (
 "encoding/json"
 "errors"
 "os"
 "github.com/SuperMarioYL/tokenctl/internal/budget"
 "github.com/SuperMarioYL/tokenctl/internal/config"
)
func main(){
 cfg:=&config.GroupConfig{Name:"demo",Weight:100,Budget:&config.TokenBudget{Tokens:10,Window:"1h",SoftThrottleAt:1},Children:[]*config.GroupConfig{{Name:"developer",Weight:100,Budget:&config.TokenBudget{Tokens:10,Window:"1h",SoftThrottleAt:1}}}}
 tree,err:=budget.NewTree(cfg,nil);if err!=nil{panic(err)};defer tree.Close()
 tree.SetReserveEstimate(1)
 if err=tree.Bind("synthetic-key","demo.developer");err!=nil{panic(err)}
 admission,err:=tree.Admit("synthetic-key","openai","synthetic-model");if err!=nil{panic(err)}
 admission.AddInput(3);admission.AddOutput(7);admission.Release()
 _,denied:=tree.Admit("synthetic-key","openai","synthetic-model")
 if !errors.Is(denied,budget.ErrDenied){panic(denied)}
 enc:=json.NewEncoder(os.Stdout);enc.SetIndent("","  ");err=enc.Encode(map[string]any{"group":admission.GroupPath(),"supplied_input_tokens":3,"supplied_output_tokens":7,"budget_tokens":10,"next_request_denied":errors.Is(denied,budget.ErrDenied),"reason":denied.Error()});if err!=nil{panic(err)}
}
