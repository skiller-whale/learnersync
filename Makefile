skillerwhalesync: SkillerWhaleSync.java
	javac --release 17 SkillerWhaleSync.java
	docker run -v `pwd`:/app -it --rm ghcr.io/graalvm/native-image SkillerWhaleSync
