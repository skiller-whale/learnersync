FROM debian:bookworm
RUN apt update && apt -qq -y install ca-certificates

EXPOSE 9494

ARG name=SkillerWhaleSync
ARG release=latest
ARG TARGETOS
ENV TARGETOS=${TARGETOS:-linux}
ARG TARGETARCH
ENV TARGETARCH=${TARGETARCH:-amd64}

# Replace the default shell with bash
RUN rm /bin/sh && ln -s /bin/bash /bin/sh

# Download the latest release of the sync binary and make it executable
ADD https://github.com/skiller-whale/learnersync/releases/download/${release}/${name}-${TARGETOS}-${TARGETARCH} /usr/local/bin/${name}
# in development: COPY SkillerWhaleSync-linux-amd64 /usr/local/bin/SkillerWhaleSync
RUN chmod +x /usr/local/bin/$name

# Set the working directory to be the exercises dir (when sh is run)
WORKDIR /app/exercises
RUN ln -s /root/.ash_history /app/.command_history

# Clear the history on startup, and run the sync
CMD > /root/.ash_history && SkillerWhaleSync
