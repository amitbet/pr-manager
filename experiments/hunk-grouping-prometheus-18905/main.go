// Standalone experiment harness. Production parsing and prompt assembly are reused unchanged.
package main
import (
 "context"
 "encoding/json"
 "fmt"
 "os"
 "path/filepath"
 "time"
 "github.com/amitbet/pr-manager/triage"
 "github.com/amitbet/pr-manager/llm"
)
type recorder struct { Requests []llm.LLMRequest }
func (r *recorder) Name() string { return "record" }
func (r *recorder) ModelID() string { return "record" }
func (r *recorder) Call(_ context.Context, q llm.LLMRequest) (*llm.LLMResponse,error) {
 r.Requests=append(r.Requests,q)
 return &llm.LLMResponse{StopReason:llm.StopToolUse,ToolCalls:[]llm.ToolCall{{Name:q.Tools[0].Name,Arguments:map[string]any{"headline":"record only","summary":"record only","focus":[]any{},"issues":[]any{}}}}},nil
}
func check(e error) { if e!=nil { panic(e) } }
func write(p string,v any) { b,e:=json.MarshalIndent(v,"","  "); check(e);check(os.WriteFile(p,append(b,'\n'),0644)) }
func main() {
 if os.Args[1]=="call" {
  var q llm.LLMRequest; check(json.NewDecoder(os.Stdin).Decode(&q))
  m:=&llm.ClaudeCodeCLI{Model:"claude-haiku-4-5"}
  start:=time.Now();resp,err:=m.Call(context.Background(),q)
  out:=map[string]any{"model":m.ModelID(),"seconds":time.Since(start).Seconds(),"response":resp}
  if err!=nil { out["error"]=err.Error() }
  check(json.NewEncoder(os.Stdout).Encode(out));return
 }
 root:=os.Args[2];raw,e:=os.ReadFile(filepath.Join(root,"data/pr.diff"));check(e)
 src,e:=triage.FromDiff(string(raw),filepath.Join(root,"data/head"),filepath.Join(root,"data/base"));check(e)
 policy:=triage.DefaultPolicy();rec:=&recorder{}
 p:=triage.Pipeline{Presorter:&triage.Presorter{Policy:policy},Summarizer:&triage.Summarizer{LLM:rec,Policy:policy},Concurrency:1,ReviewConcurrency:1}
 units:=p.Run(context.Background(),src)
 type exported struct { ID string `json:"id"`;File string `json:"file"`;Symbol string `json:"symbol"`;Diff string `json:"diff"`; Context string `json:"context"`;Hunks []triage.Hunk `json:"hunks"`;Decision triage.Decision `json:"decision"` }
 out:=[]exported{}
 for _,u:=range units { out=append(out,exported{u.ID,u.File,u.Symbol,u.Diff(),u.ReviewContext,u.Hunks,u.Decision}) }
 write(filepath.Join(root,"data/units.json"),out);write(filepath.Join(root,"data/baseline-requests.json"),rec.Requests)
 fmt.Printf("%d files, %d units, %d review requests\n",len(src.Files),len(units),len(rec.Requests))
}
