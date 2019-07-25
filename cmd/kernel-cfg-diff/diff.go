package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"sort"
	"strings"
)

var (
	configDistri = flag.String("config_distri",
		"",
		"Path to the distri kernel config")

	configOther = flag.String("config_other",
		"",
		"Path to the other kernel config")
)

func parse(dest map[string]string, b []byte) error {
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasSuffix(line, "=y") {
			dest[strings.TrimSuffix(line, "=y")] = "y"
		} else if strings.HasSuffix(line, "=m") {
			dest[strings.TrimSuffix(line, "=m")] = "m"
		}
	}
	return nil
}

func allOpts(cfgDistri, cfgOther map[string]string) []string {
	present := make(map[string]bool)
	for k := range cfgDistri {
		present[k] = true
	}
	for k := range cfgOther {
		present[k] = true
	}
	opts := make([]string, 0, len(present))
	for k := range present {
		opts = append(opts, k)
	}
	sort.Strings(opts)
	return opts
}

func logic(configDistri, configOther string) error {
	bDistri, err := ioutil.ReadFile(configDistri)
	if err != nil {
		return err
	}
	bOther, err := ioutil.ReadFile(configOther)
	if err != nil {
		return err
	}
	cfgDistri := make(map[string]string)
	cfgOther := make(map[string]string)
	if err := parse(cfgDistri, bDistri); err != nil {
		return err
	}
	if err := parse(cfgOther, bOther); err != nil {
		return err
	}
	for _, opt := range allOpts(cfgDistri, cfgOther) {
		if cfgDistri[opt] != "" && cfgOther[opt] == "" {
			fmt.Printf("only in distri: %v=%v\n", opt, cfgDistri[opt])
			continue
		}
		if cfgDistri[opt] == "" && cfgOther[opt] != "" {
			fmt.Printf("only in other: %v=%v\n", opt, cfgOther[opt])
			continue
		}
		if cfgDistri[opt] == cfgOther[opt] {
			continue
		}
		if cfgDistri[opt] == "y" && cfgOther[opt] == "m" {
			log.Printf("FYI: distri y/other m: %v", opt)
			continue // distri is more strict
		}
		fmt.Printf("diff: %v=%v (distri) vs. %v (other)\n", opt, cfgDistri[opt], cfgOther[opt])
		continue
	}
	return nil
}

func main() {
	flag.Parse()
	if err := logic(*configDistri, *configOther); err != nil {
		log.Fatal(err)
	}
}
