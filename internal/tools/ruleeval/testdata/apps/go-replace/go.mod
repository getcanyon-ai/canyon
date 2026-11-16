module example.com/replace

go 1.22

require github.com/lib/pq v1.10.9

replace github.com/lib/pq => github.com/acme/pq v1.0.0
