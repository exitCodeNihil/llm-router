# Java workspace: Temurin 21 JDK, Maven and Gradle. ~/.m2 and ~/.gradle sit
# under $HOME on the volume, so dependency downloads are cached across restarts.
FROM eclipse-temurin:21-jdk-jammy

RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates openssh-client curl less zsh unzip maven \
 && rm -rf /var/lib/apt/lists/*

ARG GRADLE_VERSION=8.14
RUN curl -fsSL -o /tmp/gradle.zip https://services.gradle.org/distributions/gradle-${GRADLE_VERSION}-bin.zip \
 && unzip -q /tmp/gradle.zip -d /opt && rm /tmp/gradle.zip \
 && ln -s /opt/gradle-${GRADLE_VERSION}/bin/gradle /usr/local/bin/gradle

RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @earendil-works/pi-coding-agent
RUN curl -fsSL https://code-server.dev/install.sh | sh

RUN sed -i 's|^root:x:0:0:root:/root:.*|root:x:0:0:root:/workspace/.home:/usr/bin/zsh|' /etc/passwd
ENV HOME=/workspace/.home SHELL=/usr/bin/zsh
ENV MAVEN_CONFIG=/workspace/.home/.m2 GRADLE_USER_HOME=/workspace/.home/.gradle
WORKDIR /workspace

CMD ["code-server", "--bind-addr", "0.0.0.0:8080", "--auth", "none", "--disable-telemetry", "/workspace"]
