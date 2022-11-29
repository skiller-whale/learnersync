FROM debian:bookworm

ARG name=SkillerWhaleSync
ARG version=0.1.2
ARG osarch=linux-amd64

# RUN apt update && apt -qq -y install curl
RUN rm /bin/sh && ln -s /bin/bash /bin/sh

ADD https://github.com/skiller-whale/learnersync/releases/download/v${version}/${name}-${osarch} /usr/local/bin/${name}
# in development: COPY SkillerWhaleSync-linux-amd64 /usr/local/bin/SkillerWhaleSync
RUN chmod +x /usr/local/bin/$name


# Set the working directory to be the exercises dir (when sh is run)
WORKDIR /app/exercises
RUN ln -s /root/.ash_history /app/.command_history

# Clear the history on startup, and run the sync
CMD > /root/.ash_history && SkillerWhaleSync
