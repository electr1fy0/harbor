# broken-build

The build command for this fixture is ./build.sh, which exits non-zero on
purpose. It lets later builder tests assert BUILD_FAILED without making the
repository itself fail go test ./....
