# idea: apt-cachealot

## problem
i need to spin up debian based docker-container often. i often load the same packages over a somewhat slow internet connection.

## proposal

it should be fast. meaning a fast delivery, for already downloaded assets: a zero copy streaming.
for not already downloaded assets: download for cache and stream simultanios, dont first download than deliver.

apt should be able to connect in 2 ways: either the traditional proxa way, eg setting `Acquire::http::Proxy "http://proxy.example.com:3142";` 

or by simple prefixing the url with the caches hostname and port, eg
`http://apt-cachealot.example.com:3142/deb.debian.org/debian/dists/bullseye/main/binary-amd64/Packages.gz` instead of
`http://deb.debian.org/debian/dists/bullseye/main/binary-amd64/Packages.gz`

`http://apt-cachealot.example.com:3142/deb.debian.org:1234/debian/dists/bullseye/main/binary-amd64/Packages.gz` instead of
`http://deb.debian.org:1234/debian/dists/bullseye/main/binary-amd64/Packages.gz`


it should deduplicate the assets, meaning the same .deb downloaded from 2 different urls should be stored only once, and the cache should be able to serve it from the cache.

.debs should neer be revalidated, just the index should be revalidated with an configurabel interval.

cache should not be cleared by a timeout, but by a maximal storage size. once the maximum storage is reached the assett that was delivered the longest time ago should be deleted first. (or maybe a more sophisticated algorithm, like LRU)


