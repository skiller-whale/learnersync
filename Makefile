skillerwhalesync: SkillerWhaleSync.java
	javac SkillerWhaleSync.java
	docker run -v `pwd`:/app -it --rm ghcr.io/graalvm/native-image SkillerWhaleSync
