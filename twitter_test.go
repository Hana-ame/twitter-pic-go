package twitter

import (
	"fmt"
	"testing"
)

func Test1(t *testing.T) {
	str, e := curlMetaData("lulu463098")

	fmt.Println(e)
	fmt.Println(str)
}
