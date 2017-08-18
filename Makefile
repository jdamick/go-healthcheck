
PACKAGES:=$(shell go list ./... | grep -v /vendor/|grep -v /cov)

.PHONY: test
test:
	go test -race -v $(PACKAGES)

.PHONY: coverage
coverage: bin/gocovmerge
	mkdir -p .coverage
	rm -f .coverage/*.out .coverage/all.merged
	for MOD in $(PACKAGES); do \
		go test -coverpkg=`echo $(COV_PKGS)|tr " " ","` -coverprofile=.coverage/unit-`echo $$MOD|tr "/" "_"`.out $$MOD 2>&1 | grep -v "no packages being tested depend on"; \
	done
	gocovmerge .coverage/*.out > .coverage/all.merged
	go tool cover -html .coverage/all.merged -o .coverage/all.html
	@echo ""
	go tool cover -func .coverage/all.merged	

.PHONY: bin/gocovmerge
bin/gocovmerge:
	go install ./cov/github.com/wadey/gocovmerge
