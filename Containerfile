FROM docker.io/library/node:20-alpine AS build

WORKDIR /app

RUN corepack enable

COPY package.json yarn.lock .yarnrc.yml ./
RUN yarn install --immutable

COPY . .
RUN yarn build

FROM docker.io/library/nginx:1.27-alpine

RUN chmod -R 777 /var/cache/nginx /var/run /var/log/nginx /usr/share/nginx/html /etc/nginx

COPY --from=build /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/nginx.conf

USER 1001

ENTRYPOINT ["nginx", "-g", "daemon off;"]
