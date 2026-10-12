package upstream

import (
	"fmt"
	"testing"
)

func TestDbgClassify(t *testing.T) {
	tr := &traeSSETranslator{}
	ue := tr.classifyStreamError("{\"code\":1001,\"message\":\"auth expired\"}")
	fmt.Println("kind:", ue.Kind, "msg:", ue.Msg)
	tr2 := &traeSSETranslator{rc: traeFakeStream(
		"event:progress_notice\ndata: {\"name\":\"construct_prompt\"}\n",
		"event:error\ndata: {\"code\":1001,\"message\":\"auth expired\"}\n",
	), cancel: func() {}}
	ue2 := tr2.preread()
	fmt.Println("preread:", ue2)
}
