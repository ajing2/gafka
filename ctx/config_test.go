package ctx

import (
	"testing"

	"github.com/funkygao/assert"
)

func TestLoadConfig(t *testing.T) {
	LoadConfig("gafka.cf")
	t.Logf("%+v", conf)
	assert.Equal(t, 1, len(conf.zones))
	assert.Equal(t, "info", conf.logLevel)
	alias, present := Alias("localtopics")
	assert.Equal(t, true, present)
	assert.Equal(t, "topics -z local", alias)
	alias, present = Alias("non-existent")
	assert.Equal(t, false, present)
	assert.Equal(t, "", alias)

	host, present := ReverseDnsLookup("127.0.0.1", 0)
	assert.Equal(t, true, present)
	assert.Equal(t, "k10121a.demo.com", host)

}
